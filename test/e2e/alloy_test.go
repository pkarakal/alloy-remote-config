//go:build e2e

// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/composition"
)

const (
	// A change reaches Alloy within one 10s poll; allow several polls plus apply time.
	reloadTimeout = 60 * time.Second
	pollInterval  = 2 * time.Second
	// statusPoll samples binding status fast: Alloy reports an apply status once
	// per change, and the operator shows it until the next poll (about 10s).
	statusPoll = 500 * time.Millisecond
	// settleFor is how long a "must not change" assertion has to hold: two polls.
	settleFor = 25 * time.Second
)

// marker is a zero-dependency component that Alloy lists by its ID, so specs
// can tell which config is running without any external backend.
func marker(name string) string {
	return fmt.Sprintf("prometheus.exporter.self %q { }\n", name)
}

func markerID(name string) string { return "prometheus.exporter.self." + name }

// expectRunning waits until the Alloy runs exactly the given top-level markers
// and has loaded its remote config successfully.
func expectRunning(a *alloyInstance, markers ...string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		ids := a.ComponentIDs(g, "")
		for _, m := range markers {
			g.Expect(ids).To(ContainElement(markerID(m)))
		}
		g.Expect(ids).To(HaveLen(len(markers)))
		g.Expect(a.Metric(g, metricLastLoadSuccessful)).To(BeEquivalentTo(1))
	}, reloadTimeout, pollInterval).Should(Succeed())
}

var _ = Describe("Alloy remote config", func() {
	It("serves the default pipeline to a collector no binding targets", func() {
		id := uniqueID()
		a := deployAlloy(id, alloyOptions{Attributes: map[string]string{"cluster_id": id}})
		a.WaitReady()

		// marker_default comes from the chart's default PipelineConfig.
		expectRunning(a, "marker_default")
	})

	// The specs share one Alloy and one PipelineConfig and each builds on the
	// state the previous one left: v1, then v2, then a bad config on top of v2.
	Describe("tenant-bound config lifecycle", Ordered, func() {
		var (
			id, cfg, bind string
			alloy         *alloyInstance
		)

		BeforeAll(func() {
			id = uniqueID()
			cfg, bind = "cfg-"+id, "bind-"+id
			pipelineConfig(cfg, marker("marker_v1"))
			binding(bindingSpec{Name: bind, Tenant: id, Pipeline: cfg})
			alloy = deployAlloy(id, alloyOptions{Attributes: map[string]string{"tenant": id}})
			alloy.WaitReady()
		})

		It("registers the tenant and loads the bound pipeline", func() {
			Eventually(func(g Gomega) {
				b := getBinding(g, bind)
				g.Expect(b.Status.AppliedCount).To(BeNumerically(">=", 1))
				g.Expect(b.Status.FailedCount).To(BeZero())
				g.Expect(b.Status.RegisteredTenants).To(ContainElement(
					And(
						HaveField("ID", id),
						HaveField("ConfigStatus", fleetv1alpha1.ConfigStatusApplied),
					)))
			}, reloadTimeout, statusPoll).Should(Succeed())

			expectRunning(alloy, "marker_v1")
		})

		It("reloads when the bound PipelineConfig changes", func() {
			applyPipelineConfig(cfg, marker("marker_v2"))
			expectRunning(alloy, "marker_v2")
		})

		It("keeps the last good config when a bad one is served", func() {
			var loadedBefore []string
			Eventually(func(g Gomega) { loadedBefore = alloy.LoadedHashes(g) }).Should(Succeed())
			Expect(loadedBefore).NotTo(BeEmpty())

			// The operator does not validate syntax; Alloy rejects the unknown component.
			applyPipelineConfig(cfg, "no.such.component \"bad\" { }\n")

			Eventually(func(g Gomega) {
				b := getBinding(g, bind)
				g.Expect(b.Status.FailedCount).To(BeNumerically(">=", 1))
				g.Expect(b.Status.LastApplyError).To(ContainSubstring("no.such.component"))
			}, reloadTimeout, statusPoll).Should(Succeed())
			Eventually(alloy.Logs, reloadTimeout, pollInterval).Should(
				ContainSubstring("failed to parse and load new remote configuration"))

			// Alloy keeps running v2 and does not crash or fall back to nothing.
			Consistently(func(g Gomega) {
				g.Expect(alloy.ComponentIDs(g, "")).To(ConsistOf(markerID("marker_v2")))
				// No new config was loaded successfully.
				g.Expect(alloy.LoadedHashes(g)).To(Equal(loadedBefore))
			}, settleFor, pollInterval).Should(Succeed())
		})
	})

	It("lets a cohort binding outrank a tenant binding only for matching collectors", func() {
		id := uniqueID()
		other := uniqueID()
		tenantCfg, cohortCfg := "tenant-"+id, "cohort-"+id
		group := "cohort-" + id

		pipelineConfig(tenantCfg, marker("marker_tenant"))
		pipelineConfig(cohortCfg, marker("marker_cohort"))
		collectorGroup(group, fmt.Sprintf(`cluster_id="%s"`, id))
		// Tenant bindings default to priority 200, so 300 outranks them.
		binding(bindingSpec{Name: "tenant-" + id, Tenant: id, Pipeline: tenantCfg})
		binding(bindingSpec{Name: "cohort-" + id, Group: group, Pipeline: cohortCfg, Priority: 300})

		// Both collectors share the tenant; only the first is in the cohort.
		member := deployAlloy(id, alloyOptions{Attributes: map[string]string{"tenant": id, "cluster_id": id}})
		outsider := deployAlloy(other, alloyOptions{Attributes: map[string]string{"tenant": id, "cluster_id": other}})
		member.WaitReady()
		outsider.WaitReady()

		expectRunning(member, "marker_cohort")
		expectRunning(outsider, "marker_tenant")
	})

	It("adds an additive pipeline next to the base and removes it on expiry", func() {
		id := uniqueID()
		baseCfg, extraCfg := "base-"+id, "extra-"+id
		group, extraBinding := "cohort-"+id, "extra-"+id
		// The additive runs inside a custom component whose label derives from the binding name.
		custom := composition.Label(extraBinding) + ".default"
		module := "remotecfg/" + custom

		pipelineConfig(baseCfg, marker("marker_base"))
		pipelineConfig(extraCfg, marker("marker_extra"))
		collectorGroup(group, fmt.Sprintf(`cluster_id="%s"`, id))
		binding(bindingSpec{Name: "base-" + id, Tenant: id, Pipeline: baseCfg})
		extra := bindingSpec{Name: extraBinding, Group: group, Pipeline: extraCfg, Additive: true}
		binding(extra)

		a := deployAlloy(id, alloyOptions{Attributes: map[string]string{"tenant": id, "cluster_id": id}})
		a.WaitReady()

		By("running the base marker and the additive's marker inside its declare instance")
		Eventually(func(g Gomega) {
			g.Expect(a.ComponentIDs(g, "")).To(ContainElements(markerID("marker_base"), custom))
			g.Expect(a.ComponentIDs(g, module)).To(ConsistOf(markerID("marker_extra")))
		}, reloadTimeout, pollInterval).Should(Succeed())

		By("recording the tenant on the additive binding as well as the base")
		Eventually(func(g Gomega) {
			g.Expect(getBinding(g, extraBinding).Status.RegisteredTenants).To(ContainElement(HaveField("ID", id)))
		}, reloadTimeout, pollInterval).Should(Succeed())

		By("expiring the additive binding")
		extra.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		applyBinding(extra)
		Eventually(func(g Gomega) {
			ids := a.ComponentIDs(g, "")
			g.Expect(ids).To(ConsistOf(markerID("marker_base")))
		}, reloadTimeout, pollInterval).Should(Succeed())
	})
})

// Trusted attributes make the manager take an attribute from a proxy-set
// header instead of the collector's own claim. Enabling the flag restarts the
// manager for every spec, so it lives in its own Ordered container that
// upgrades the release first and restores it afterwards. Specs run serially,
// so no other spec sees the flag.
var _ = Describe("Trusted attributes", Ordered, func() {
	BeforeAll(func() {
		installChart("--set-json", `manager.args=["--trusted-attribute=tenant=X-Tenant"]`)
		DeferCleanup(func() { installChart() })
	})

	It("takes the tenant from the header when the collector claims none", func() {
		id := uniqueID()
		cfg, bind := "cfg-"+id, "bind-"+id
		pipelineConfig(cfg, marker("marker_trusted"))
		binding(bindingSpec{Name: bind, Tenant: id, Pipeline: cfg})

		a := deployAlloy(id, alloyOptions{Headers: map[string]string{"X-Tenant": id}})
		a.WaitReady()

		expectRunning(a, "marker_trusted")
		Eventually(func(g Gomega) {
			g.Expect(getBinding(g, bind).Status.RegisteredTenants).To(ContainElement(HaveField("ID", id)))
		}, reloadTimeout, pollInterval).Should(Succeed())
	})

	It("rejects a collector that claims another tenant than its header proves", func() {
		victim, attacker := uniqueID(), uniqueID()
		cfg, bind := "cfg-"+victim, "bind-"+victim
		pipelineConfig(cfg, marker("marker_victim"))
		binding(bindingSpec{Name: bind, Tenant: victim, Pipeline: cfg})

		// Alloy exits when its first fetch fails, so it never becomes ready.
		a := deployAlloy(attacker, alloyOptions{
			Attributes: map[string]string{"tenant": victim},
			Headers:    map[string]string{"X-Tenant": attacker},
		})

		Eventually(a.Logs, reloadTimeout, pollInterval).Should(
			ContainSubstring("does not match the authenticated identity"))
		Consistently(func(g Gomega) {
			g.Expect(getBinding(g, bind).Status.RegisteredTenants).To(BeEmpty())
		}, 10*time.Second, pollInterval).Should(Succeed())
	})
})
