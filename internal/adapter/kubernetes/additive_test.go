// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"crypto/sha256"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/composition"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

var _ = Describe("Additive pipelines and expiry", func() {
	const (
		namespace  = "default"
		baseConfig = "local.file \"base\" {\n  filename = \"/tmp/base\"\n}\n"
		debugConf  = "local.file \"debug\" {\n  filename = \"/tmp/debug\"\n}"
		canaryConf = "local.file \"canary\" {\n  filename = \"/tmp/canary\"\n}\n"
	)

	var (
		clock                  time.Time
		cohortAttrs, plainAttr map[string]string
		pcNames                = []string{"ap-pc-base", "ap-pc-debug", "ap-pc-bad", "ap-pc-canary"}
		bindingNames           = []string{"ap-base", "ap-debug", "ap-bad", "ap-canary", "ap-orphan"}
	)

	hashOf := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

	createPC := func(name, content string) {
		pc := &fleetv1alpha1.PipelineConfig{
			Name: name, Namespace: namespace, Spec: fleetv1alpha1.PipelineConfigSpec{Content: content},
		}
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		pc.Status.ContentHash = hashOf(content)
		Expect(k8sClient.Status().Update(ctx, pc)).To(Succeed())
	}

	createBinding := func(name, tenant, group, pc string, mod func(*fleetv1alpha1.CollectorGroupBindingSpec)) {
		spec := fleetv1alpha1.CollectorGroupBindingSpec{TenantRef: tenant, CollectorGroupRef: group, PipelineConfigRef: pc}
		if mod != nil {
			mod(&spec)
		}
		Expect(k8sClient.Create(ctx, &fleetv1alpha1.CollectorGroupBinding{
			Name: name, Namespace: namespace, Spec: spec,
		})).To(Succeed())
	}

	additive := func(s *fleetv1alpha1.CollectorGroupBindingSpec) { s.Mode = fleetv1alpha1.BindingModeAdditive }
	expires := func(at time.Time) func(*fleetv1alpha1.CollectorGroupBindingSpec) {
		return func(s *fleetv1alpha1.CollectorGroupBindingSpec) {
			t := metav1.NewTime(at)
			s.ExpiresAt = &t
		}
	}

	cleanup := func() {
		for _, n := range bindingNames {
			b := &fleetv1alpha1.CollectorGroupBinding{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, b); err == nil {
				b.Finalizers = nil
				_ = k8sClient.Update(ctx, b)
				_ = k8sClient.Delete(ctx, b)
			}
		}
		for _, n := range pcNames {
			pc := &fleetv1alpha1.PipelineConfig{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, pc); err == nil {
				pc.Finalizers = nil
				_ = k8sClient.Update(ctx, pc)
				_ = k8sClient.Delete(ctx, pc)
			}
		}
		g := &fleetv1alpha1.CollectorGroup{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ap-cluster-1", Namespace: namespace}, g); err == nil {
			g.Finalizers = nil
			_ = k8sClient.Update(ctx, g)
			_ = k8sClient.Delete(ctx, g)
		}
	}

	BeforeEach(func() {
		cleanup()
		clock = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		cohortAttrs = map[string]string{"tenant": "ap-tenant", "cluster_id": "c-1"}
		plainAttr = map[string]string{"tenant": "ap-tenant", "cluster_id": "c-2"}
		createPC("ap-pc-base", baseConfig)
		createPC("ap-pc-debug", debugConf)
		createPC("ap-pc-bad", "local.file \"x\" {")
		createPC("ap-pc-canary", canaryConf)
		Expect(k8sClient.Create(ctx, &fleetv1alpha1.CollectorGroup{
			Name: "ap-cluster-1", Namespace: namespace,
			Spec: fleetv1alpha1.CollectorGroupSpec{Matchers: []string{`cluster_id="c-1"`}},
		})).To(Succeed())
		createBinding("ap-base", "ap-tenant", "", "ap-pc-base", nil)
	})

	AfterEach(cleanup)

	newResolver := func() *ConfigResolver {
		return NewConfigResolver(mgrClient, WithResolverClock(func() time.Time { return clock }))
	}

	resolve := func(r *ConfigResolver, attrs map[string]string) (*port.ResolvedConfig, error) {
		return r.Resolve(ctx, namespace, attrs)
	}

	It("composes a debug pipeline only for its cohort and returns to the base on expiry", func() {
		createBinding("ap-debug", "", "ap-cluster-1", "ap-pc-debug",
			func(s *fleetv1alpha1.CollectorGroupBindingSpec) {
				additive(s)
				expires(clock.Add(time.Hour))(s)
			})
		r := newResolver()
		composed := composition.Compose(baseConfig, hashOf(baseConfig),
			[]composition.Additive{{BindingName: "ap-debug", Content: debugConf}})

		Eventually(func(g Gomega) {
			got, err := resolve(r, cohortAttrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Content).To(Equal(composed.Content))
			g.Expect(got.ContentHash).To(Equal(composed.Hash))
			g.Expect(got.Additives).To(Equal([]string{"ap-debug"}))
			g.Expect(got.Path).To(Equal(port.PathTenant))
			g.Expect(got.BindingName).To(Equal("ap-base"))

			other, err := resolve(r, plainAttr)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(other.Content).To(Equal(baseConfig))
			g.Expect(other.ContentHash).To(Equal(hashOf(baseConfig)))
			g.Expect(other.Additives).To(BeEmpty())
		}).Should(Succeed())

		By("moving the clock past expiresAt")
		clock = clock.Add(2 * time.Hour)
		got, err := resolve(r, cohortAttrs)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ContentHash).To(Equal(hashOf(baseConfig)))
		Expect(got.Content).To(Equal(baseConfig))
	})

	It("falls back to the next candidate when a Replace canary expires", func() {
		createBinding("ap-canary", "ap-tenant", "", "ap-pc-canary", func(s *fleetv1alpha1.CollectorGroupBindingSpec) {
			p := int32(900)
			s.Priority = &p
			expires(clock.Add(time.Hour))(s)
		})
		r := newResolver()
		Eventually(func(g Gomega) {
			got, err := resolve(r, cohortAttrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Content).To(Equal(canaryConf))
			g.Expect(got.BindingName).To(Equal("ap-canary"))
		}).Should(Succeed())

		clock = clock.Add(2 * time.Hour)
		got, err := resolve(r, cohortAttrs)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Content).To(Equal(baseConfig))
		Expect(got.BindingName).To(Equal("ap-base"))
	})

	It("leaves out an additive whose content does not parse", func() {
		createBinding("ap-bad", "", "ap-cluster-1", "ap-pc-bad", additive)
		createBinding("ap-debug", "", "ap-cluster-1", "ap-pc-debug", additive)
		r := newResolver()
		composed := composition.Compose(baseConfig, hashOf(baseConfig),
			[]composition.Additive{{BindingName: "ap-debug", Content: debugConf}})
		Eventually(func(g Gomega) {
			got, err := resolve(r, cohortAttrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.ContentHash).To(Equal(composed.Hash))
			g.Expect(got.Additives).To(Equal([]string{"ap-debug"}))
		}).Should(Succeed())
	})

	It("serves the base alone when every additive is invalid", func() {
		createBinding("ap-bad", "", "ap-cluster-1", "ap-pc-bad", additive)
		r := newResolver()
		Eventually(func(g Gomega) {
			g.Expect(mgrClient.Get(ctx, types.NamespacedName{Name: "ap-bad", Namespace: namespace},
				&fleetv1alpha1.CollectorGroupBinding{})).To(Succeed())
			got, err := resolve(r, cohortAttrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Content).To(Equal(baseConfig))
			g.Expect(got.ContentHash).To(Equal(hashOf(baseConfig)))
			g.Expect(got.Additives).To(BeEmpty())
		}).Should(Succeed())
	})

	It("serves the additives alone when there is no base, and not found when none is usable", func() {
		createBinding("ap-orphan", "ap-orphan-tenant", "", "ap-pc-debug", additive)
		r := newResolver()
		attrs := map[string]string{"tenant": "ap-orphan-tenant"}
		want := composition.Compose("", "", []composition.Additive{{BindingName: "ap-orphan", Content: debugConf}})
		Eventually(func(g Gomega) {
			got, err := resolve(r, attrs)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Content).To(Equal(want.Content))
			g.Expect(got.ContentHash).To(Equal(want.Hash))
			g.Expect(got.Path).To(Equal(port.PathAdditive))
		}).Should(Succeed())

		b := &fleetv1alpha1.CollectorGroupBinding{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ap-orphan", Namespace: namespace}, b)).To(Succeed())
		past := metav1.NewTime(clock.Add(-time.Hour))
		b.Spec.ExpiresAt = &past
		Expect(k8sClient.Update(ctx, b)).To(Succeed())
		Eventually(func() error {
			_, err := resolve(r, attrs)
			return err
		}).Should(MatchError(port.ErrConfigNotFound))
	})

	It("does not let an additive mask a failing base", func() {
		createBinding("ap-debug", "", "ap-cluster-1", "ap-pc-debug", additive)
		Expect(k8sClient.Delete(ctx, &fleetv1alpha1.PipelineConfig{Name: "ap-pc-base", Namespace: namespace})).To(Succeed())
		r := newResolver()
		Eventually(func() error {
			_, err := resolve(r, cohortAttrs)
			return err
		}).Should(MatchError(port.ErrConfigNotReady))
	})

	It("registers the collector on the base and on each contributing additive", func() {
		createBinding("ap-debug", "", "ap-cluster-1", "ap-pc-debug", additive)
		createBinding("ap-bad", "", "ap-cluster-1", "ap-pc-bad", additive)
		registry := NewKubernetesCollectorRegistry(mgrClient, k8sClient, namespace,
			WithClock(func() time.Time { return clock }))

		Eventually(func(g Gomega) {
			g.Expect(registry.Register(ctx, port.CollectorInfo{ID: "ap-collector", LocalAttributes: cohortAttrs})).To(Succeed())
			for _, name := range []string{"ap-base", "ap-debug"} {
				b := &fleetv1alpha1.CollectorGroupBinding{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, b)).To(Succeed())
				g.Expect(b.Status.RegisteredTenants).To(HaveLen(1), name)
				g.Expect(b.Status.RegisteredTenants[0].CollectorID).To(Equal("ap-collector"))
			}
		}).Should(Succeed())

		bad := &fleetv1alpha1.CollectorGroupBinding{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ap-bad", Namespace: namespace}, bad)).To(Succeed())
		Expect(bad.Status.RegisteredTenants).To(BeEmpty())

		By("recording a failed apply on both bindings")
		Eventually(func(g Gomega) {
			g.Expect(registry.Heartbeat(ctx, port.CollectorInfo{
				ID: "ap-collector", LocalAttributes: cohortAttrs,
				ConfigStatus: port.ConfigStatusFailed, ConfigError: "boom",
			})).To(Succeed())
			for _, name := range []string{"ap-base", "ap-debug"} {
				b := &fleetv1alpha1.CollectorGroupBinding{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, b)).To(Succeed())
				g.Expect(b.Status.RegisteredTenants[0].ConfigStatus).To(Equal(fleetv1alpha1.ConfigStatusFailed), name)
			}
		}).Should(Succeed())

		By("unregistering from every binding that holds the collector")
		Eventually(func(g Gomega) {
			g.Expect(registry.Unregister(ctx, "ap-collector", port.UnregisterScope{})).To(Succeed())
			for _, name := range []string{"ap-base", "ap-debug"} {
				b := &fleetv1alpha1.CollectorGroupBinding{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, b)).To(Succeed())
				g.Expect(b.Status.RegisteredTenants).To(BeEmpty(), name)
			}
		}).Should(Succeed())
	})
})
