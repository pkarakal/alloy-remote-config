// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

var _ = Describe("CollectorGroup Controller", func() {
	const resourceName = "test-collectorgroup"
	const namespace = "default"

	ctx := context.Background()

	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: namespace,
	}

	newReconciler := func() *CollectorGroupReconciler {
		return &CollectorGroupReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	}

	Context("When the resource does not exist", func() {
		It("should return no error for a missing CollectorGroup", func() {
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "nonexistent",
					Namespace: namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When reconciling a new CollectorGroup", func() {
		BeforeEach(func() {
			By("creating the CollectorGroup resource")
			cg := &fleetv1alpha1.CollectorGroup{}
			err := k8sClient.Get(ctx, typeNamespacedName, cg)
			if err != nil {
				resource := &fleetv1alpha1.CollectorGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: namespace,
					},
					Spec: fleetv1alpha1.CollectorGroupSpec{
						Description: "Test collector group",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("deleting the CollectorGroup resource")
			cg := &fleetv1alpha1.CollectorGroup{}
			err := k8sClient.Get(ctx, typeNamespacedName, cg)
			if err == nil {
				// Remove finalizer so the resource can be deleted
				cg.Finalizers = nil
				Expect(k8sClient.Update(ctx, cg)).To(Succeed())
				Expect(k8sClient.Delete(ctx, cg)).To(Succeed())
			}
		})

		It("should add the finalizer on first reconciliation", func() {
			By("reconciling the newly created resource")
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the finalizer was added")
			cg := &fleetv1alpha1.CollectorGroup{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, cg)).To(Succeed())
			Expect(cg.Finalizers).To(ContainElement(CollectorGroupFinalizer))
		})

		It("should update ObservedGeneration in status after finalizer is present", func() {
			By("doing the first reconcile to add the finalizer")
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("doing the second reconcile to update status")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying ObservedGeneration is set in status")
			cg := &fleetv1alpha1.CollectorGroup{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, cg)).To(Succeed())
			Expect(cg.Status.ObservedGeneration).To(Equal(cg.Generation))
		})
	})

	Context("When deleting a CollectorGroup", func() {
		BeforeEach(func() {
			By("creating the CollectorGroup with the finalizer already set")
			resource := &fleetv1alpha1.CollectorGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:       resourceName,
					Namespace:  namespace,
					Finalizers: []string{CollectorGroupFinalizer},
				},
				Spec: fleetv1alpha1.CollectorGroupSpec{
					Description: "Test collector group",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			By("ensuring the CollectorGroup is cleaned up")
			cg := &fleetv1alpha1.CollectorGroup{}
			err := k8sClient.Get(ctx, typeNamespacedName, cg)
			if err != nil {
				return
			}
			// Remove finalizer so K8s can complete deletion.
			// If DeletionTimestamp is already set, removing the finalizer
			// triggers immediate removal — no explicit Delete needed.
			cg.Finalizers = nil
			Expect(k8sClient.Update(ctx, cg)).To(Succeed())
			if cg.DeletionTimestamp.IsZero() {
				Expect(k8sClient.Delete(ctx, cg)).To(Succeed())
			}
		})

		It("should block deletion and requeue when active bindings exist", func() {
			By("setting active bindings in status")
			cg := &fleetv1alpha1.CollectorGroup{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, cg)).To(Succeed())
			cg.Status.ActiveBindings = 2
			Expect(k8sClient.Status().Update(ctx, cg)).To(Succeed())

			By("triggering deletion")
			Expect(k8sClient.Delete(ctx, cg)).To(Succeed())

			By("reconciling the resource under deletion")
			reconciler := newReconciler()
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying the finalizer is still present")
			updated := &fleetv1alpha1.CollectorGroup{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(CollectorGroupFinalizer))
		})

		It("should remove the finalizer and allow deletion when no active bindings exist", func() {
			By("ensuring active bindings is zero")
			cg := &fleetv1alpha1.CollectorGroup{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, cg)).To(Succeed())
			Expect(cg.Status.ActiveBindings).To(Equal(0))

			By("triggering deletion")
			Expect(k8sClient.Delete(ctx, cg)).To(Succeed())

			By("reconciling the resource under deletion")
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the finalizer was removed")
			updated := &fleetv1alpha1.CollectorGroup{}
			err = k8sClient.Get(ctx, typeNamespacedName, updated)
			// Resource may already be gone once the finalizer is removed
			if err == nil {
				Expect(updated.Finalizers).NotTo(ContainElement(CollectorGroupFinalizer))
			}
		})
	})
})
