// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"crypto/sha256"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/port"
	"github.com/pkarakal/alloy-remote-config/internal/selection"
)

var _ = Describe("ConfigResolver", func() {
	const namespace = "default"

	cleanup := func(names ...string) {
		for _, name := range names {
			pc := &fleetv1alpha1.PipelineConfig{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pc); err == nil {
				pc.Finalizers = nil
				_ = k8sClient.Update(ctx, pc)
				_ = k8sClient.Delete(ctx, pc)
			}
			binding := &fleetv1alpha1.CollectorGroupBinding{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, binding); err == nil {
				binding.Finalizers = nil
				_ = k8sClient.Update(ctx, binding)
				_ = k8sClient.Delete(ctx, binding)
			}
		}
	}

	createPipelineConfig := func(name, content string, labels map[string]string) {
		pc := &fleetv1alpha1.PipelineConfig{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
			Spec: fleetv1alpha1.PipelineConfigSpec{
				Content: content,
			},
		}
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())

		sum := sha256.Sum256([]byte(content))
		pc.Status.ContentHash = fmt.Sprintf("%x", sum)
		Expect(k8sClient.Status().Update(ctx, pc)).To(Succeed())
	}

	createBinding := func(name string, tenantRef, collectorGroupRef, pipelineConfigRef string) {
		binding := &fleetv1alpha1.CollectorGroupBinding{
			Name:      name,
			Namespace: namespace,
			Spec: fleetv1alpha1.CollectorGroupBindingSpec{
				TenantRef:         tenantRef,
				CollectorGroupRef: collectorGroupRef,
				PipelineConfigRef: pipelineConfigRef,
			},
		}
		Expect(k8sClient.Create(ctx, binding)).To(Succeed())
	}

	Context("ResolveByTenant", func() {
		AfterEach(func() {
			cleanup("tenant-pc", "tenant-binding")
		})

		It("should resolve config for a matching tenant binding", func() {
			createPipelineConfig("tenant-pc", "tenant pipeline content", nil)
			createBinding("tenant-binding", "my-tenant", "", "tenant-pc")

			resolver := NewConfigResolver(mgrClient)
			Eventually(func(g Gomega) {
				resolved, err := resolver.ResolveByTenant(ctx, namespace, "my-tenant")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(resolved.Content).To(Equal("tenant pipeline content"))
				g.Expect(resolved.ContentHash).NotTo(BeEmpty())
			}).Should(Succeed())
		})

		It("should return ErrConfigNotFound for unknown tenant", func() {
			resolver := NewConfigResolver(mgrClient)
			_, err := resolver.ResolveByTenant(ctx, namespace, "nonexistent-tenant")
			Expect(err).To(MatchError(port.ErrConfigNotFound))
		})
	})

	Context("ResolveByCollectorGroup", func() {
		AfterEach(func() {
			cleanup("group-pc", "group-binding")
		})

		It("should resolve config for a matching collector group binding", func() {
			createPipelineConfig("group-pc", "group pipeline content", nil)
			createBinding("group-binding", "", "my-group", "group-pc")

			resolver := NewConfigResolver(mgrClient)
			Eventually(func(g Gomega) {
				resolved, err := resolver.ResolveByCollectorGroup(ctx, namespace, "my-group")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(resolved.Content).To(Equal("group pipeline content"))
			}).Should(Succeed())
		})

		It("should return ErrConfigNotFound for unknown collector group", func() {
			resolver := NewConfigResolver(mgrClient)
			_, err := resolver.ResolveByCollectorGroup(ctx, namespace, "nonexistent-group")
			Expect(err).To(MatchError(port.ErrConfigNotFound))
		})
	})

	Context("ResolveDefault", func() {
		AfterEach(func() {
			cleanup("default", "default-unreconciled")
		})

		It("should resolve the default PipelineConfig by label", func() {
			createPipelineConfig("default", "default pipeline content",
				map[string]string{fleetv1alpha1.LabelDefaultPipelineConfig: "true"})

			resolver := NewConfigResolver(mgrClient)
			Eventually(func(g Gomega) {
				resolved, err := resolver.ResolveDefault(ctx, namespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(resolved.Content).To(Equal("default pipeline content"))
			}).Should(Succeed())
		})

		It("should return ErrConfigNotFound when no default exists", func() {
			resolver := NewConfigResolver(mgrClient)
			_, err := resolver.ResolveDefault(ctx, namespace)
			Expect(err).To(MatchError(port.ErrConfigNotFound))
		})

		It("should return ErrConfigNotReady when default exists but has no hash", func() {
			pc := &fleetv1alpha1.PipelineConfig{
				Name:      "default",
				Namespace: namespace,
				Labels:    map[string]string{fleetv1alpha1.LabelDefaultPipelineConfig: "true"},
				Spec: fleetv1alpha1.PipelineConfigSpec{
					Content: "not yet reconciled",
				},
			}
			Expect(k8sClient.Create(ctx, pc)).To(Succeed())

			resolver := NewConfigResolver(mgrClient)
			Eventually(func(g Gomega) {
				_, err := resolver.ResolveDefault(ctx, namespace)
				g.Expect(err).To(MatchError(port.ErrConfigNotReady))
			}).Should(Succeed())
		})
	})

	Context("dangling binding", func() {
		AfterEach(func() {
			cleanup("dangling-binding", "dangling-fallback")
		})

		It("should return ErrConfigNotReady when the referenced PipelineConfig is missing", func() {
			createBinding("dangling-binding", "dangling-tenant", "", "does-not-exist")

			resolver := NewConfigResolver(mgrClient)
			Eventually(func(g Gomega) {
				_, err := resolver.ResolveByTenant(ctx, namespace, "dangling-tenant")
				g.Expect(err).To(MatchError(port.ErrConfigNotReady))
				g.Expect(err.Error()).To(ContainSubstring("does-not-exist"))
			}).Should(Succeed())
		})
	})

	Context("deterministic selection", func() {
		AfterEach(func() {
			cleanup("pick-pc-a", "pick-pc-b", "pick-bind-a", "pick-bind-b", "default-a", "default-b")
		})

		// expectedWinner mirrors the documented rule on the stored timestamps, so
		// the test holds whether or not creation crossed a second boundary.
		expectedWinner := func(names ...string) string {
			objs := make([]client.Object, 0, len(names))
			for _, n := range names {
				b := &fleetv1alpha1.CollectorGroupBinding{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, b)).To(Succeed())
				objs = append(objs, b)
			}
			winner := objs[0]
			for _, o := range objs[1:] {
				if selection.Older(o, winner) {
					winner = o
				}
			}
			return winner.GetName()
		}

		It("should make the resolver and the registry pick the same binding", func() {
			createPipelineConfig("pick-pc-a", "content a", nil)
			createPipelineConfig("pick-pc-b", "content b", nil)
			createBinding("pick-bind-b", "pick-tenant", "", "pick-pc-b")
			createBinding("pick-bind-a", "pick-tenant", "", "pick-pc-a")
			winner := expectedWinner("pick-bind-a", "pick-bind-b")
			wantContent := map[string]string{"pick-bind-a": "content a", "pick-bind-b": "content b"}[winner]

			resolver := NewConfigResolver(mgrClient)
			registry := NewKubernetesCollectorRegistry(mgrClient, namespace)
			Eventually(func(g Gomega) {
				resolved, err := resolver.ResolveByTenant(ctx, namespace, "pick-tenant")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(resolved.Content).To(Equal(wantContent))

				found, err := registry.findBinding(ctx, "pick-tenant", "")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).NotTo(BeNil())
				g.Expect(found.Name).To(Equal(winner))
			}).Should(Succeed())
		})

		It("should pick the same default PipelineConfig on every call", func() {
			labels := map[string]string{fleetv1alpha1.LabelDefaultPipelineConfig: "true"}
			createPipelineConfig("default-b", "default b", labels)
			createPipelineConfig("default-a", "default a", labels)
			names := map[string]string{"default-a": "default a", "default-b": "default b"}
			var winner string
			a, b := &fleetv1alpha1.PipelineConfig{}, &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "default-a", Namespace: namespace}, a)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "default-b", Namespace: namespace}, b)).To(Succeed())
			winner = "default-a"
			if selection.Older(b, a) {
				winner = "default-b"
			}

			resolver := NewConfigResolver(mgrClient)
			Eventually(func(g Gomega) {
				for range 5 {
					resolved, err := resolver.ResolveDefault(ctx, namespace)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(resolved.Content).To(Equal(names[winner]))
				}
			}).Should(Succeed())
		})
	})

	DescribeTable("toPipelineResult",
		func(content, hash string, wantErr error) {
			pc := &fleetv1alpha1.PipelineConfig{
				Spec:   fleetv1alpha1.PipelineConfigSpec{Content: content},
				Status: fleetv1alpha1.PipelineConfigStatus{ContentHash: hash},
			}
			resolved, err := NewConfigResolver(nil).toPipelineResult(pc)
			if wantErr != nil {
				Expect(err).To(MatchError(wantErr))
				Expect(resolved).To(BeNil())
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved.Content).To(Equal(content))
			Expect(resolved.ContentHash).To(Equal(hash))
		},
		Entry("reconciled content", "real", "abc", nil),
		Entry("no hash yet", "real", "", port.ErrConfigNotReady),
		Entry("stale hash on empty content", "", "abc", port.ErrConfigNotReady),
		Entry("stale hash on whitespace content", " \n\t", "abc", port.ErrConfigNotReady),
	)
})
