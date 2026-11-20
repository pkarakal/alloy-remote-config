//go:build e2e

// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/test/utils"
)

// kindCluster is the dedicated Kind cluster every command targets.
func kindCluster() string {
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok && v != "" {
		return v
	}
	return "kind"
}

// kubeContext is the only kubeconfig context the suite may use.
func kubeContext() string { return "kind-" + kindCluster() }

// kubectl runs kubectl pinned to the Kind context, so a stray current-context
// can never receive a command.
func kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", append([]string{"--context", kubeContext()}, args...)...))
}

// kubectlStdin is kubectl with a manifest on stdin.
func kubectlStdin(stdin string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--context", kubeContext()}, args...)...)
	cmd.Stdin = bytes.NewBufferString(stdin)
	return utils.Run(cmd)
}

// helm runs helm pinned to the Kind context.
func helm(args ...string) (string, error) {
	return utils.Run(exec.Command("helm", append([]string{"--kube-context", kubeContext()}, args...)...))
}

// apply creates or updates the objects in a manifest.
func apply(manifest string) {
	GinkgoHelper()
	out, err := kubectlStdin(manifest, "apply", "-n", testNamespace, "-f", "-")
	Expect(err).NotTo(HaveOccurred(), out)
}

// deleteOnCleanup deletes an object after the spec. Register it before the
// objects that depend on it: Ginkgo runs cleanups last-in first-out, so
// dependents are removed first.
func deleteOnCleanup(kind, name string) {
	DeferCleanup(func() {
		_, _ = kubectl("delete", kind, name, "-n", testNamespace,
			"--ignore-not-found", "--wait=true", "--timeout=60s")
	})
}

// uniqueID returns a short DNS-label-safe id so specs never share names or
// collector attributes.
func uniqueID() string {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	Expect(err).NotTo(HaveOccurred())
	return "e" + hex.EncodeToString(b)
}

// pipelineConfig applies a PipelineConfig and removes it after the spec.
func pipelineConfig(name, content string) {
	GinkgoHelper()
	deleteOnCleanup("pipelineconfigs.fleet.pkarakal.com", name)
	applyPipelineConfig(name, content)
}

// applyPipelineConfig creates or updates a PipelineConfig.
func applyPipelineConfig(name, content string) {
	GinkgoHelper()
	apply(fmt.Sprintf(`apiVersion: fleet.pkarakal.com/v1alpha1
kind: PipelineConfig
metadata:
  name: %s
spec:
  content: |
%s
`, name, indent(content, "    ")))
}

// collectorGroup applies a CollectorGroup with matchers and removes it after the spec.
func collectorGroup(name string, matchers ...string) {
	GinkgoHelper()
	deleteOnCleanup("collectorgroups.fleet.pkarakal.com", name)
	var sb strings.Builder
	for _, m := range matchers {
		fmt.Fprintf(&sb, "  - %q\n", m)
	}
	apply(fmt.Sprintf(`apiVersion: fleet.pkarakal.com/v1alpha1
kind: CollectorGroup
metadata:
  name: %s
spec:
  matchers:
%s`, name, sb.String()))
}

// bindingSpec is the subset of CollectorGroupBinding.spec the specs set.
type bindingSpec struct {
	Name      string
	Group     string
	Tenant    string
	Pipeline  string
	Priority  int
	Additive  bool
	ExpiresAt string
}

// binding applies a CollectorGroupBinding and removes it after the spec.
func binding(b bindingSpec) {
	GinkgoHelper()
	deleteOnCleanup("collectorgroupbindings.fleet.pkarakal.com", b.Name)
	applyBinding(b)
}

// applyBinding creates or updates a CollectorGroupBinding.
func applyBinding(b bindingSpec) {
	GinkgoHelper()
	var sb strings.Builder
	fmt.Fprintf(&sb, "apiVersion: fleet.pkarakal.com/v1alpha1\nkind: CollectorGroupBinding\nmetadata:\n  name: %s\nspec:\n", b.Name)
	fmt.Fprintf(&sb, "  pipelineConfigRef: %s\n", b.Pipeline)
	if b.Group != "" {
		fmt.Fprintf(&sb, "  collectorGroupRef: %s\n", b.Group)
	}
	if b.Tenant != "" {
		fmt.Fprintf(&sb, "  tenantRef: %s\n", b.Tenant)
	}
	if b.Priority != 0 {
		fmt.Fprintf(&sb, "  priority: %d\n", b.Priority)
	}
	if b.Additive {
		sb.WriteString("  mode: Additive\n")
	}
	if b.ExpiresAt != "" {
		fmt.Fprintf(&sb, "  expiresAt: %q\n", b.ExpiresAt)
	}
	apply(sb.String())
}

// getBinding reads a CollectorGroupBinding.
func getBinding(g Gomega, name string) fleetv1alpha1.CollectorGroupBinding {
	out, err := kubectl("get", "collectorgroupbindings.fleet.pkarakal.com", name, "-n", testNamespace, "-o", "json")
	g.Expect(err).NotTo(HaveOccurred())
	var b fleetv1alpha1.CollectorGroupBinding
	g.Expect(json.Unmarshal([]byte(out), &b)).To(Succeed())
	return b
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
