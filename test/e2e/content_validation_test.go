//go:build e2e

// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package e2e

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

// invalidSyntax does not parse: the block is never closed. Unknown components
// parse fine and are only rejected by Alloy itself.
const invalidSyntax = "prometheus.exporter.self \"broken\" {\n"

// tryApplyPipelineConfig is applyPipelineConfig that returns the kubectl
// output (stderr included, so admission warnings show) instead of asserting.
func tryApplyPipelineConfig(name, content string) (string, error) {
	return kubectlStdin(fmt.Sprintf(`apiVersion: fleet.pkarakal.com/v1alpha1
kind: PipelineConfig
metadata:
  name: %s
spec:
  content: |
%s
`, name, indent(content, "    ")), "apply", "-n", testNamespace, "-f", "-")
}

// getPipelineConfig reads a PipelineConfig.
func getPipelineConfig(g Gomega, name string) fleetv1alpha1.PipelineConfig {
	out, err := kubectl("get", "pipelineconfigs.fleet.pkarakal.com", name, "-n", testNamespace, "-o", "json")
	g.ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var pc fleetv1alpha1.PipelineConfig
	g.ExpectWithOffset(1, json.Unmarshal([]byte(out), &pc)).To(Succeed())
	return pc
}

func contentValid(pc fleetv1alpha1.PipelineConfig) *metav1.Condition {
	for i := range pc.Status.Conditions {
		if pc.Status.Conditions[i].Type == fleetv1alpha1.ConditionTypeContentValid {
			return &pc.Status.Conditions[i]
		}
	}
	return nil
}

// boundAlloy applies a PipelineConfig carrying one marker, binds it to a fresh
// tenant and returns an Alloy running it.
func boundAlloy(markerName string) (cfg, bind string, a *alloyInstance) {
	GinkgoHelper()
	id := uniqueID()
	cfg, bind = "cfg-"+id, "bind-"+id
	pipelineConfig(cfg, marker(markerName))
	binding(bindingSpec{Name: bind, Tenant: id, Pipeline: cfg})
	a = deployAlloy(id, alloyOptions{Attributes: map[string]string{"tenant": id}})
	a.WaitReady()
	expectRunning(a, markerName)
	return cfg, bind, a
}

// The chart default is enforce with the webhook on and failurePolicy Fail.
var _ = Describe("Content validation with the default enforce webhook", func() {
	It("rejects invalid content on create and on edit, and Alloy keeps running", func() {
		cfg, bind, a := boundAlloy("marker_ok")

		By("rejecting an invalid edit of the served PipelineConfig")
		out, err := tryApplyPipelineConfig(cfg, invalidSyntax)
		Expect(err).To(HaveOccurred())
		Expect(out).To(And(ContainSubstring("spec.content"), ContainSubstring("Alloy syntax error")))

		By("rejecting a new invalid PipelineConfig")
		fresh := "fresh-" + uniqueID()
		deleteOnCleanup("pipelineconfigs.fleet.pkarakal.com", fresh)
		out, err = tryApplyPipelineConfig(fresh, invalidSyntax)
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("Alloy syntax error"))
		out, err = kubectl("get", "pipelineconfigs.fleet.pkarakal.com", fresh, "-n", testNamespace)
		Expect(err).To(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("NotFound"))

		By("leaving the running config and the binding untouched")
		Consistently(func(g Gomega) {
			g.Expect(a.ComponentIDs(g, "")).To(ConsistOf(markerID("marker_ok")))
			g.Expect(getBinding(g, bind).Status.FailedCount).To(BeZero())
			g.Expect(getPipelineConfig(g, cfg).Spec.Content).To(ContainSubstring("marker_ok"))
		}, settleFor, pollInterval).Should(Succeed())

		By("still admitting a valid edit")
		applyPipelineConfig(cfg, marker("marker_next"))
		expectRunning(a, "marker_next")
	})
})

var _ = Describe("Content validation in warn mode", Ordered, func() {
	BeforeAll(func() {
		installChart("--set", "manager.contentValidation=warn")
		DeferCleanup(func() { installChart() })
	})

	It("admits invalid content with a warning, flags it, and Alloy reports the failure", func() {
		cfg, bind, a := boundAlloy("marker_ok")

		// Webhook calls can fail briefly right after the upgrade; retry until it answers.
		var out string
		Eventually(func(g Gomega) {
			var err error
			out, err = tryApplyPipelineConfig(cfg, invalidSyntax)
			g.Expect(err).NotTo(HaveOccurred(), out)
			g.Expect(out).To(ContainSubstring("Warning:"))
		}, reloadTimeout, pollInterval).Should(Succeed())
		Expect(out).To(And(ContainSubstring("Alloy syntax error"), ContainSubstring("content-validation=warn")))

		Eventually(func(g Gomega) {
			c := contentValid(getPipelineConfig(g, cfg))
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal("SyntaxError"))
		}, reloadTimeout, pollInterval).Should(Succeed())

		// warn still serves it, so Alloy rejects it and keeps the last good config.
		Eventually(func(g Gomega) {
			g.Expect(getBinding(g, bind).Status.FailedCount).To(BeNumerically(">=", 1))
		}, reloadTimeout, pollInterval).Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(a.ComponentIDs(g, "")).To(ConsistOf(markerID("marker_ok")))
		}, settleFor, pollInterval).Should(Succeed())
	})
})

// Without the webhook an invalid edit gets through, which is what the
// reconciler-side protection is for.
var _ = Describe("Content validation without the webhook", Ordered, func() {
	var (
		cfg, bind string
		alloy     *alloyInstance
	)

	BeforeAll(func() {
		installChart("--set", "webhook.enabled=false")
		DeferCleanup(func() { installChart() })
		cfg, bind, alloy = boundAlloy("marker_ok")
	})

	It("enforce keeps serving the last valid content and Alloy never sees the bad edit", func() {
		var hashesBefore []string
		Eventually(func(g Gomega) { hashesBefore = alloy.LoadedHashes(g) }).Should(Succeed())

		applyPipelineConfig(cfg, invalidSyntax)

		Eventually(func(g Gomega) {
			pc := getPipelineConfig(g, cfg)
			c := contentValid(pc)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal("SyntaxError"))
			g.Expect(pc.Status.LastValidContent).To(ContainSubstring("marker_ok"))
		}, reloadTimeout, pollInterval).Should(Succeed())

		// Several polls: the bad content is never served, so nothing fails or reloads.
		Consistently(func(g Gomega) {
			g.Expect(alloy.ComponentIDs(g, "")).To(ConsistOf(markerID("marker_ok")))
			g.Expect(alloy.LoadedHashes(g)).To(Equal(hashesBefore))
			b := getBinding(g, bind)
			g.Expect(b.Status.FailedCount).To(BeZero())
			g.Expect(b.Status.LastApplyError).To(BeEmpty())
		}, settleFor, pollInterval).Should(Succeed())

		By("a valid edit takes over again")
		applyPipelineConfig(cfg, marker("marker_v2"))
		expectRunning(alloy, "marker_v2")
		Eventually(func(g Gomega) {
			c := contentValid(getPipelineConfig(g, cfg))
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		}, reloadTimeout, pollInterval).Should(Succeed())
	})

	It("warn serves the invalid content and Alloy itself keeps its last good config", func() {
		installChart("--set", "webhook.enabled=false", "--set", "manager.contentValidation=warn")

		applyPipelineConfig(cfg, invalidSyntax)

		Eventually(func(g Gomega) {
			b := getBinding(g, bind)
			g.Expect(b.Status.FailedCount).To(BeNumerically(">=", 1))
			g.Expect(b.Status.LastApplyError).NotTo(BeEmpty())
		}, reloadTimeout, pollInterval).Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(alloy.ComponentIDs(g, "")).To(ConsistOf(markerID("marker_v2")))
		}, settleFor, pollInterval).Should(Succeed())
	})
})
