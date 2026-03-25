// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"crypto/sha256"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/port"
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
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    labels,
			},
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
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
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
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: namespace,
					Labels:    map[string]string{fleetv1alpha1.LabelDefaultPipelineConfig: "true"},
				},
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
})
