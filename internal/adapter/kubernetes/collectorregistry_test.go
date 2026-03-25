// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

var _ = Describe("KubernetesCollectorRegistry", func() {
	const namespace = "default"

	// newRegistry creates a registry backed by the cache-aware mgrClient so
	// field-index queries work, while Status patches go directly to the API server.
	newRegistry := func() *CollectorRegistry {
		return NewKubernetesCollectorRegistry(mgrClient, namespace)
	}

	makeInfo := func(id, tenant, collectorGroup string) port.CollectorInfo {
		return port.CollectorInfo{
			ID:   id,
			Name: id,
			LocalAttributes: map[string]string{
				"tenant":          tenant,
				"collector_group": collectorGroup,
			},
		}
	}

	deleteBinding := func(name string) {
		b := &fleetv1alpha1.CollectorGroupBinding{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, b); err == nil {
			b.Finalizers = nil
			_ = k8sClient.Update(ctx, b)
			_ = k8sClient.Delete(ctx, b)
		}
	}

	makeBinding := func(name, tenantRef, collectorGroupRef string) *fleetv1alpha1.CollectorGroupBinding {
		b := &fleetv1alpha1.CollectorGroupBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: fleetv1alpha1.CollectorGroupBindingSpec{
				TenantRef:         tenantRef,
				CollectorGroupRef: collectorGroupRef,
				PipelineConfigRef: "some-pipeline",
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, b)).To(Succeed())
		return b
	}

	Context("Register", func() {
		Context("when a tenant-specific binding exists", func() {
			const bindingName = "reg-tenant-binding"
			AfterEach(func() { deleteBinding(bindingName) })

			It("upserts the tenant into the binding status", func() {
				makeBinding(bindingName, "acme", "")
				reg := newRegistry()

				// Use Eventually so the cache has time to sync the new binding.
				Eventually(func(g Gomega) {
					g.Expect(reg.Register(ctx, makeInfo("collector-1", "acme", "prod"))).To(Succeed())
					b := &fleetv1alpha1.CollectorGroupBinding{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName, Namespace: namespace}, b)).To(Succeed())
					g.Expect(b.Status.RegisteredTenants).To(HaveLen(1))
					g.Expect(b.Status.RegisteredTenants[0].ID).To(Equal("acme"))
					g.Expect(b.Status.RegisteredTenants[0].CollectorID).To(Equal("collector-1"))
					g.Expect(b.Status.RegisteredTenants[0].LastSeenAt.IsZero()).To(BeFalse())
				}).Should(Succeed())
			})
		})

		Context("when only a group-wide binding exists", func() {
			const bindingName = "reg-group-binding"
			AfterEach(func() { deleteBinding(bindingName) })

			It("upserts the tenant into the group binding status", func() {
				makeBinding(bindingName, "", "prod")
				reg := newRegistry()

				Eventually(func(g Gomega) {
					g.Expect(reg.Register(ctx, makeInfo("collector-2", "contoso", "prod"))).To(Succeed())
					b := &fleetv1alpha1.CollectorGroupBinding{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName, Namespace: namespace}, b)).To(Succeed())
					g.Expect(b.Status.RegisteredTenants).To(HaveLen(1))
					g.Expect(b.Status.RegisteredTenants[0].ID).To(Equal("contoso"))
				}).Should(Succeed())
			})
		})

		Context("when no binding exists", func() {
			It("returns nil without mutating any resource", func() {
				reg := newRegistry()
				Expect(reg.Register(ctx, makeInfo("collector-3", "unknown-tenant", "unknown-group"))).To(Succeed())
			})
		})

		Context("when the tenant attribute is empty", func() {
			It("returns an error", func() {
				reg := newRegistry()
				info := port.CollectorInfo{ID: "collector-4", LocalAttributes: map[string]string{}}
				Expect(reg.Register(ctx, info)).To(HaveOccurred())
			})
		})

		Context("when the same tenant registers twice", func() {
			const bindingName = "reg-upsert-binding"
			AfterEach(func() { deleteBinding(bindingName) })

			It("upserts (exactly one entry, LastSeenAt updated)", func() {
				makeBinding(bindingName, "fabrikam", "")
				reg := newRegistry()

				var firstSeen metav1.Time
				Eventually(func(g Gomega) {
					g.Expect(reg.Register(ctx, makeInfo("collector-5", "fabrikam", ""))).To(Succeed())
					b := &fleetv1alpha1.CollectorGroupBinding{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName, Namespace: namespace}, b)).To(Succeed())
					g.Expect(b.Status.RegisteredTenants).To(HaveLen(1))
					firstSeen = b.Status.RegisteredTenants[0].LastSeenAt
				}).Should(Succeed())

				// Second registration — same tenant, same binding.
				Eventually(func(g Gomega) {
					g.Expect(reg.Register(ctx, makeInfo("collector-5", "fabrikam", ""))).To(Succeed())
					b := &fleetv1alpha1.CollectorGroupBinding{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName, Namespace: namespace}, b)).To(Succeed())
					g.Expect(b.Status.RegisteredTenants).To(HaveLen(1))
					g.Expect(b.Status.RegisteredTenants[0].LastSeenAt.Equal(&firstSeen) ||
						b.Status.RegisteredTenants[0].LastSeenAt.After(firstSeen.Time)).To(BeTrue())
				}).Should(Succeed())
			})
		})

		Context("when two different tenants register to the same group binding", func() {
			const bindingName = "reg-multi-tenant-binding"
			AfterEach(func() { deleteBinding(bindingName) })

			It("stores both entries", func() {
				makeBinding(bindingName, "", "shared-group")
				reg := newRegistry()

				Eventually(func(g Gomega) {
					g.Expect(reg.Register(ctx, makeInfo("collector-a", "tenant-a", "shared-group"))).To(Succeed())
					b := &fleetv1alpha1.CollectorGroupBinding{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName, Namespace: namespace}, b)).To(Succeed())
					g.Expect(b.Status.RegisteredTenants).To(HaveLen(1))
				}).Should(Succeed())

				Eventually(func(g Gomega) {
					g.Expect(reg.Register(ctx, makeInfo("collector-b", "tenant-b", "shared-group"))).To(Succeed())
					b := &fleetv1alpha1.CollectorGroupBinding{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName, Namespace: namespace}, b)).To(Succeed())
					g.Expect(b.Status.RegisteredTenants).To(HaveLen(2))
				}).Should(Succeed())
			})
		})
	})

	Context("Unregister", func() {
		Context("when the collector is registered", func() {
			const bindingName = "unreg-binding"
			AfterEach(func() { deleteBinding(bindingName) })

			It("removes the entry from the binding status", func() {
				makeBinding(bindingName, "my-tenant", "")
				reg := newRegistry()

				// Register first.
				Eventually(func(g Gomega) {
					g.Expect(reg.Register(ctx, makeInfo("collector-x", "my-tenant", ""))).To(Succeed())
					b := &fleetv1alpha1.CollectorGroupBinding{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName, Namespace: namespace}, b)).To(Succeed())
					g.Expect(b.Status.RegisteredTenants).To(HaveLen(1))
				}).Should(Succeed())

				// Unregister.
				Expect(reg.Unregister(ctx, "collector-x")).To(Succeed())

				// Verify removal.
				b := &fleetv1alpha1.CollectorGroupBinding{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName, Namespace: namespace}, b)).To(Succeed())
				Expect(b.Status.RegisteredTenants).To(BeEmpty())
			})
		})

		Context("when the collector ID is not found in any binding", func() {
			It("returns nil (idempotent)", func() {
				reg := newRegistry()
				Expect(reg.Unregister(ctx, "nonexistent-collector")).To(Succeed())
			})
		})
	})
})
