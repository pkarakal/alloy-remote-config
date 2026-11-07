// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"crypto/sha256"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

var _ = Describe("Matcher cohorts and binding priority", func() {
	const namespace = "default"

	var cohortAttrs, otherAttrs, legacyAttrs map[string]string

	createPC := func(name, content string) {
		pc := &fleetv1alpha1.PipelineConfig{
			Name: name, Namespace: namespace,
			Spec: fleetv1alpha1.PipelineConfigSpec{Content: content},
		}
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		pc.Status.ContentHash = fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
		Expect(k8sClient.Status().Update(ctx, pc)).To(Succeed())
	}

	createGroup := func(name string, matchers ...string) {
		Expect(k8sClient.Create(ctx, &fleetv1alpha1.CollectorGroup{
			Name: name, Namespace: namespace,
			Spec: fleetv1alpha1.CollectorGroupSpec{Matchers: matchers},
		})).To(Succeed())
	}

	createBinding := func(name, tenant, group, pc string, priority *int32) {
		Expect(k8sClient.Create(ctx, &fleetv1alpha1.CollectorGroupBinding{
			Name: name, Namespace: namespace,
			Spec: fleetv1alpha1.CollectorGroupBindingSpec{
				TenantRef: tenant, CollectorGroupRef: group, PipelineConfigRef: pc, Priority: priority,
			},
		})).To(Succeed())
	}

	cleanup := func() {
		for _, n := range []string{"cohort-ten", "cohort-canary", "cohort-legacy"} {
			b := &fleetv1alpha1.CollectorGroupBinding{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, b); err == nil {
				b.Finalizers = nil
				_ = k8sClient.Update(ctx, b)
				_ = k8sClient.Delete(ctx, b)
			}
		}
		for _, n := range []string{"cohort-pc-ten", "cohort-pc-canary", "cohort-pc-legacy"} {
			pc := &fleetv1alpha1.PipelineConfig{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, pc); err == nil {
				pc.Finalizers = nil
				_ = k8sClient.Update(ctx, pc)
				_ = k8sClient.Delete(ctx, pc)
			}
		}
		for _, n := range []string{"canary-org-1234", "bad-matchers"} {
			g := &fleetv1alpha1.CollectorGroup{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, g); err == nil {
				g.Finalizers = nil
				_ = k8sClient.Update(ctx, g)
				_ = k8sClient.Delete(ctx, g)
			}
		}
	}

	BeforeEach(func() {
		cohortAttrs = map[string]string{"tenant": "cohort-tenant", "org_id": "org-1234", "cluster_id": "c-2"}
		otherAttrs = map[string]string{"tenant": "cohort-tenant", "org_id": "org-9999", "cluster_id": "c-2"}
		legacyAttrs = map[string]string{"tenant": "", "collector_group": "bad-matchers", "org_id": "org-1234"}
		cleanup()
		createPC("cohort-pc-ten", "tenant config")
		createPC("cohort-pc-canary", "canary config")
		createPC("cohort-pc-legacy", "legacy config")
		createBinding("cohort-ten", "cohort-tenant", "", "cohort-pc-ten", nil)
	})

	AfterEach(cleanup)

	contentFor := func(r *ConfigResolver, attrs map[string]string) (*port.ResolvedConfig, error) {
		return r.Resolve(ctx, namespace, attrs)
	}

	It("lets a priority 300 cohort outrank a tenant binding only for its members", func() {
		createGroup("canary-org-1234", `org_id="org-1234"`, `cluster_id=~"c-1|c-2"`)
		p := int32(300)
		createBinding("cohort-canary", "", "canary-org-1234", "cohort-pc-canary", &p)

		r := NewConfigResolver(mgrClient)
		Eventually(func(g Gomega) {
			got, err := contentFor(r, cohortAttrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Content).To(Equal("canary config"))
			g.Expect(got.Path).To(Equal(port.PathCollectorGroup))
			g.Expect(got.BindingName).To(Equal("cohort-canary"))

			got, err = contentFor(r, otherAttrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Content).To(Equal("tenant config"))
			g.Expect(got.Path).To(Equal(port.PathTenant))
		}).Should(Succeed())
	})

	It("keeps the tenant binding on top when the cohort binding sets no priority", func() {
		createGroup("canary-org-1234", `org_id="org-1234"`)
		createBinding("cohort-canary", "", "canary-org-1234", "cohort-pc-canary", nil)

		r := NewConfigResolver(mgrClient)
		Consistently(func(g Gomega) {
			got, err := contentFor(r, cohortAttrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Content).To(Equal("tenant config"))
		}, "1s", "100ms").Should(Succeed())
	})

	It("falls back to legacy membership when the group's matchers are invalid", func() {
		createGroup("bad-matchers", `org_id="org-1234"`, `env=~"("`)
		p := int32(300)
		createBinding("cohort-legacy", "", "bad-matchers", "cohort-pc-legacy", &p)

		r := NewConfigResolver(mgrClient)
		Eventually(func(g Gomega) {
			// The matchers would match this collector, but they are invalid.
			_, err := contentFor(r, map[string]string{"org_id": "org-1234", "env": "x"})
			g.Expect(err).To(MatchError(port.ErrConfigNotFound))

			got, err := contentFor(r, legacyAttrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Content).To(Equal("legacy config"))
		}).Should(Succeed())
	})

	It("registers the collector against the binding resolution picks", func() {
		createGroup("canary-org-1234", `org_id="org-1234"`, `cluster_id=~"c-1|c-2"`)
		p := int32(300)
		createBinding("cohort-canary", "", "canary-org-1234", "cohort-pc-canary", &p)

		registry := NewKubernetesCollectorRegistry(mgrClient, k8sClient, namespace)
		Eventually(func(g Gomega) {
			found, err := registry.findBinding(ctx, cohortAttrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).NotTo(BeNil())
			g.Expect(found.Name).To(Equal("cohort-canary"))

			found, err = registry.findBinding(ctx, otherAttrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).NotTo(BeNil())
			g.Expect(found.Name).To(Equal("cohort-ten"))
		}).Should(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(registry.Register(ctx, port.CollectorInfo{ID: "cohort-collector", LocalAttributes: cohortAttrs})).To(Succeed())
			b := &fleetv1alpha1.CollectorGroupBinding{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cohort-canary", Namespace: namespace}, b)).To(Succeed())
			g.Expect(b.Status.RegisteredTenants).To(HaveLen(1))
			g.Expect(b.Status.RegisteredTenants[0].CollectorID).To(Equal("cohort-collector"))
		}).Should(Succeed())
	})
})
