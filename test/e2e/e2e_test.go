//go:build e2e

// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package e2e

import (
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/pkarakal/alloy-remote-config/test/utils"
)

// metricsServiceName is the metrics Service rendered by the chart.
const metricsServiceName = managerName + "-metrics-service"

// metricsRoleBindingName is the ClusterRoleBinding the spec creates to read metrics.
const metricsRoleBindingName = "arc-e2e-metrics-binding"

// metricsReaderRole is the ClusterRole rendered by the chart for metrics access.
const metricsReaderRole = "arc-metrics-reader"

var _ = Describe("Manager", func() {
	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should run the controller-manager pod", func() {
		Eventually(func(g Gomega) {
			out, err := kubectl("get", "pods", "-n", testNamespace, "-l", "control-plane=controller-manager",
				"-o", "jsonpath={range .items[*]}{.metadata.name}={.status.phase}{\"\\n\"}{end}")
			g.Expect(err).NotTo(HaveOccurred())
			lines := utils.GetNonEmptyLines(out)
			g.Expect(lines).To(HaveLen(1), "expected 1 controller pod")
			g.Expect(lines[0]).To(HaveSuffix("=Running"))
		}).Should(Succeed())
	})

	It("should serve metrics", func() {
		DeferCleanup(func() {
			_, _ = kubectl("delete", "pod", "curl-metrics", "-n", testNamespace, "--ignore-not-found", "--wait=false")
			_, _ = kubectl("delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
		})

		By("granting the service account access to metrics")
		out, err := kubectl("create", "clusterrolebinding", metricsRoleBindingName,
			"--clusterrole="+metricsReaderRole,
			fmt.Sprintf("--serviceaccount=%s:%s", testNamespace, managerName))
		Expect(err).NotTo(HaveOccurred(), out)

		By("requesting a service account token")
		var token string
		Eventually(func(g Gomega) {
			out, err := kubectlStdin(`{"apiVersion":"authentication.k8s.io/v1","kind":"TokenRequest"}`,
				"create", "--raw",
				fmt.Sprintf("/api/v1/namespaces/%s/serviceaccounts/%s/token", testNamespace, managerName),
				"-f", "-")
			g.Expect(err).NotTo(HaveOccurred())
			var tr struct {
				Status struct {
					Token string `json:"token"`
				} `json:"status"`
			}
			g.Expect(json.Unmarshal([]byte(out), &tr)).To(Succeed())
			token = tr.Status.Token
			g.Expect(token).NotTo(BeEmpty())
		}).Should(Succeed())

		By("curling the metrics endpoint from inside the cluster")
		out, err = kubectl("run", "curl-metrics", "--restart=Never", "-n", testNamespace,
			"--image="+curlImage, "--overrides", fmt.Sprintf(`{
				"spec": {
					"containers": [{
						"name": "curl",
						"image": "%s",
						"command": ["/bin/sh", "-c"],
						"args": ["for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"],
						"securityContext": {
							"readOnlyRootFilesystem": true,
							"allowPrivilegeEscalation": false,
							"capabilities": {"drop": ["ALL"]},
							"runAsNonRoot": true,
							"runAsUser": 1000,
							"seccompProfile": {"type": "RuntimeDefault"}
						}
					}],
					"serviceAccountName": "%s"
				}
			}`, curlImage, token, metricsServiceName, testNamespace, managerName))
		Expect(err).NotTo(HaveOccurred(), out)

		Eventually(func(g Gomega) {
			phase, err := kubectl("get", "pod", "curl-metrics", "-n", testNamespace, "-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Succeeded"))
		}, 5*time.Minute).Should(Succeed())

		logs, err := kubectl("logs", "curl-metrics", "-n", testNamespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("< HTTP/1.1 200 OK"))
		Expect(logs).To(ContainSubstring("controller_runtime_reconcile_total"))
	})
})
