//go:build e2e

// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	// alloyHTTPPort is Alloy's HTTP server port in the e2e Deployment.
	alloyHTTPPort = 12345
	// pollFrequency is the remotecfg poll interval. 10s is Alloy's minimum.
	pollFrequency = "10s"

	// Alloy remotecfg metrics (internal/service/remotecfg/metrics.go in grafana/alloy).
	metricLastLoadSuccessful = "remotecfg_last_load_successful"
	// remotecfg_hash has one series per config hash Alloy ever loaded successfully.
	metricLoadedHashes = "remotecfg_hash"
)

// alloyOptions configures one e2e Alloy.
type alloyOptions struct {
	// Attributes are the remotecfg attributes the collector reports.
	Attributes map[string]string
	// Headers are extra HTTP headers on every remotecfg request.
	Headers map[string]string
}

// alloyInstance is a real Alloy that fetches its config from the operator.
type alloyInstance struct {
	ID string
}

// deployAlloy starts an Alloy with a remotecfg-only local config and removes
// it after the spec. Alloy exits when its first fetch fails, so create the
// bindings it needs before calling this.
func deployAlloy(id string, opts alloyOptions) *alloyInstance {
	GinkgoHelper()
	deleteOnCleanup("deployment", "alloy-"+id)
	deleteOnCleanup("configmap", "alloy-"+id)

	var attrs, headers strings.Builder
	for k, v := range opts.Attributes {
		fmt.Fprintf(&attrs, "%q = %q, ", k, v)
	}
	if len(opts.Headers) > 0 {
		headers.WriteString("  http_headers = {")
		for k, v := range opts.Headers {
			fmt.Fprintf(&headers, "%q = [%q], ", k, v)
		}
		headers.WriteString("}\n")
	}
	alloyConfig := fmt.Sprintf(`remotecfg {
  url = "http://%s.%s.svc.cluster.local:12345"
  id = %q
  attributes = {%s}
  poll_frequency = %q
%s}
`, apiServiceName, testNamespace, id, attrs.String(), pollFrequency, headers.String())

	apply(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: alloy-%[1]s
data:
  config.alloy: |
%[2]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: alloy-%[1]s
  labels:
    e2e-alloy: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      e2e-alloy: %[1]s
  template:
    metadata:
      labels:
        e2e-alloy: %[1]s
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: alloy
        image: %[3]s
        imagePullPolicy: Never
        args:
        - run
        - /etc/alloy/config.alloy
        - --server.http.listen-addr=0.0.0.0:%[4]d
        - --storage.path=/tmp/alloy
        - --disable-reporting
        ports:
        - name: http
          containerPort: %[4]d
        readinessProbe:
          httpGet:
            path: /-/ready
            port: http
          periodSeconds: 3
        resources:
          requests:
            cpu: 50m
            memory: 128Mi
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop: ["ALL"]
        volumeMounts:
        - name: config
          mountPath: /etc/alloy
        - name: storage
          mountPath: /tmp/alloy
      volumes:
      - name: config
        configMap:
          name: alloy-%[1]s
      - name: storage
        emptyDir: {}
`, id, indent(alloyConfig, "    "), alloyImage, alloyHTTPPort))
	return &alloyInstance{ID: id}
}

// WaitReady blocks until the Alloy pod is Ready, meaning its first fetch worked.
func (a *alloyInstance) WaitReady() {
	GinkgoHelper()
	out, err := kubectl("rollout", "status", "deployment/alloy-"+a.ID, "-n", testNamespace, "--timeout=120s")
	Expect(err).NotTo(HaveOccurred(), out)
}

// pod returns the name of the live Alloy pod.
func (a *alloyInstance) pod(g Gomega) string {
	out, err := kubectl("get", "pods", "-n", testNamespace, "-l", "e2e-alloy="+a.ID,
		"--field-selector=status.phase=Running", "-o", "jsonpath={.items[0].metadata.name}")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(out).NotTo(BeEmpty(), "no running Alloy pod yet")
	return out
}

// proxyGet fetches a path from Alloy's HTTP server through the API server pod proxy.
func (a *alloyInstance) proxyGet(g Gomega, path string) string {
	raw := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/%s", testNamespace, a.pod(g), alloyHTTPPort, path)
	out, err := kubectl("get", "--raw", raw)
	g.Expect(err).NotTo(HaveOccurred())
	return out
}

// alloyComponent is the subset of Alloy's component info the specs read.
type alloyComponent struct {
	LocalID  string `json:"localID"`
	ModuleID string `json:"moduleID"`
	Name     string `json:"name"`
	Label    string `json:"label"`
	Health   struct {
		State   string `json:"state"`
		Message string `json:"message"`
	} `json:"health"`
}

// Components lists the components of the remotecfg-supplied config. With a
// moduleID it lists the components inside that custom component instance.
func (a *alloyInstance) Components(g Gomega, moduleID string) []alloyComponent {
	path := "api/v0/web/remotecfg/components"
	if moduleID != "" {
		path = "api/v0/web/remotecfg/modules/" + moduleID + "/components"
	}
	var comps []alloyComponent
	g.Expect(json.Unmarshal([]byte(a.proxyGet(g, path)), &comps)).To(Succeed())
	return comps
}

// ComponentIDs returns the local IDs of the remotecfg components.
func (a *alloyInstance) ComponentIDs(g Gomega, moduleID string) []string {
	comps := a.Components(g, moduleID)
	ids := make([]string, 0, len(comps))
	for _, c := range comps {
		ids = append(ids, c.LocalID)
	}
	return ids
}

// Metric returns the value of an unlabeled metric from Alloy's /metrics.
func (a *alloyInstance) Metric(g Gomega, name string) float64 {
	for line := range strings.SplitSeq(a.proxyGet(g, "metrics"), "\n") {
		if rest, ok := strings.CutPrefix(line, name+" "); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			g.Expect(err).NotTo(HaveOccurred())
			return v
		}
	}
	g.Expect(false).To(BeTrue(), "metric %s not exposed", name)
	return 0
}

// LoadedHashes returns the hashes of every config this Alloy loaded successfully.
func (a *alloyInstance) LoadedHashes(g Gomega) []string {
	var hashes []string
	prefix := metricLoadedHashes + `{hash="`
	for line := range strings.SplitSeq(a.proxyGet(g, "metrics"), "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			hash, _, _ := strings.Cut(rest, `"`)
			hashes = append(hashes, hash)
		}
	}
	slices.Sort(hashes)
	return hashes
}

// Logs returns the container logs, including the previous container if it crashed.
func (a *alloyInstance) Logs() string {
	var sb strings.Builder
	for _, extra := range [][]string{{}, {"--previous"}} {
		args := append([]string{"logs", "-n", testNamespace, "-l", "e2e-alloy=" + a.ID, "--tail=200"}, extra...)
		out, _ := kubectl(args...)
		sb.WriteString(out)
	}
	return sb.String()
}
