// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
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
			Client: indexedListClient{Client: k8sClient, lister: mgrClient},
			Scheme: k8sClient.Scheme(),
		}
	}

	Context("When the resource does not exist", func() {
		It("should return no error for a missing CollectorGroup", func() {
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				Name:      "nonexistent",
				Namespace: namespace,
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
					Name:      resourceName,
					Namespace: namespace,
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

	DescribeTable("MatchersValid condition",
		func(matchers []string, wantStatus metav1.ConditionStatus, wantMsg string) {
			cg := &fleetv1alpha1.CollectorGroup{
				Name: resourceName, Namespace: namespace,
				Spec: fleetv1alpha1.CollectorGroupSpec{Matchers: matchers},
			}
			Expect(k8sClient.Create(ctx, cg)).To(Succeed())
			DeferCleanup(func() {
				c := &fleetv1alpha1.CollectorGroup{}
				if err := k8sClient.Get(ctx, typeNamespacedName, c); err == nil {
					c.Finalizers = nil
					_ = k8sClient.Update(ctx, c)
					_ = k8sClient.Delete(ctx, c)
				}
			})

			reconciler := newReconciler()
			for range 2 { // first pass adds the finalizer
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(k8sClient.Get(ctx, typeNamespacedName, cg)).To(Succeed())
			cond := meta.FindStatusCondition(cg.Status.Conditions, fleetv1alpha1.ConditionTypeMatchersValid)
			if wantStatus == "" {
				Expect(cond).To(BeNil())
				return
			}
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(wantStatus))
			Expect(cond.Message).To(ContainSubstring(wantMsg))
		},
		Entry("no matchers sets no condition", nil, metav1.ConditionStatus(""), ""),
		Entry("valid matchers", []string{`org_id="org-1234"`, `cluster_id=~"c-1|c-2"`}, metav1.ConditionTrue, "compiled"),
		Entry("regex that does not compile", []string{`org_id="x"`, `env=~"("`}, metav1.ConditionFalse, "invalid regular expression"),
	)

	It("rejects matchers with the wrong syntax at admission", func() {
		cg := &fleetv1alpha1.CollectorGroup{
			Name: "bad-syntax-group", Namespace: namespace,
			Spec: fleetv1alpha1.CollectorGroupSpec{Matchers: []string{"org_id=org-1234"}},
		}
		Expect(k8sClient.Create(ctx, cg)).NotTo(Succeed())
	})

	Context("When deleting a CollectorGroup", func() {
		BeforeEach(func() {
			By("creating the CollectorGroup with the finalizer already set")
			resource := &fleetv1alpha1.CollectorGroup{
				Name:       resourceName,
				Namespace:  namespace,
				Finalizers: []string{CollectorGroupFinalizer},
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
