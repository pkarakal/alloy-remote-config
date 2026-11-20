// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

var _ = Describe("PipelineConfig Controller", func() {
	const resourceName = "test-pipelineconfig"
	const namespace = "default"

	ctx := context.Background()

	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: namespace,
	}

	newReconciler := func() *PipelineConfigReconciler {
		return &PipelineConfigReconciler{
			Client: indexedListClient{Client: k8sClient, lister: mgrClient},
			Scheme: k8sClient.Scheme(),
		}
	}

	cleanup := func() {
		pc := &fleetv1alpha1.PipelineConfig{}
		err := k8sClient.Get(ctx, typeNamespacedName, pc)
		if err != nil {
			return
		}
		pc.Finalizers = nil
		Expect(k8sClient.Update(ctx, pc)).To(Succeed())
		if pc.DeletionTimestamp.IsZero() {
			Expect(k8sClient.Delete(ctx, pc)).To(Succeed())
		}
	}

	Context("When the resource does not exist", func() {
		It("should return no error for a missing PipelineConfig", func() {
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				Name:      "nonexistent",
				Namespace: namespace,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When reconciling a new PipelineConfig", func() {
		BeforeEach(func() {
			By("creating the PipelineConfig resource")
			pc := &fleetv1alpha1.PipelineConfig{}
			err := k8sClient.Get(ctx, typeNamespacedName, pc)
			if err != nil {
				resource := &fleetv1alpha1.PipelineConfig{
					Name:      resourceName,
					Namespace: namespace,
					Spec: fleetv1alpha1.PipelineConfigSpec{
						Content: "logging { level = \"info\" }",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(cleanup)

		It("should add the finalizer on first reconciliation", func() {
			By("reconciling the newly created resource")
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the finalizer was added")
			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pc)).To(Succeed())
			Expect(pc.Finalizers).To(ContainElement(PipelineConfigFinalizer))
		})

		It("should compute the content hash and set ContentValid condition", func() {
			By("doing the first reconcile to add the finalizer")
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("doing the second reconcile to update status")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the content hash matches")
			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pc)).To(Succeed())
			sum := sha256.Sum256([]byte(pc.Spec.Content))
			expected := fmt.Sprintf("%x", sum)
			Expect(pc.Status.ContentHash).To(Equal(expected))

			By("verifying ContentValid condition is True")
			cond := meta.FindStatusCondition(pc.Status.Conditions, typeContentValidPipelineConfig)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("ContentValid"))

			By("verifying ObservedGeneration is set")
			Expect(pc.Status.ObservedGeneration).To(Equal(pc.Generation))
		})
	})

	Context("When the CRD rejects blank content", func() {
		DescribeTable("creating a PipelineConfig with blank content fails validation",
			func(content string) {
				err := k8sClient.Create(ctx, &fleetv1alpha1.PipelineConfig{
					Name:      resourceName,
					Namespace: namespace,
					Spec:      fleetv1alpha1.PipelineConfigSpec{Content: content},
				})
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
			},
			Entry("empty", ""),
			Entry("whitespace", " \n\t"),
		)
	})

	// Objects stored before the CEL rule persist thanks to validation
	// ratcheting, so the API server cannot create them here; a fake client can.
	DescribeTable("When reconciling a pre-existing PipelineConfig with blank content",
		func(content string) {
			pc := &fleetv1alpha1.PipelineConfig{
				Name:       resourceName,
				Namespace:  namespace,
				Finalizers: []string{PipelineConfigFinalizer},
				Spec:       fleetv1alpha1.PipelineConfigSpec{Content: content},
				Status:     fleetv1alpha1.PipelineConfigStatus{ContentHash: "stale-hash"},
			}
			fc := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).
				WithObjects(pc).WithStatusSubresource(pc).
				WithIndex(&fleetv1alpha1.CollectorGroupBinding{}, IndexPipelineConfigRef,
					func(o client.Object) []string {
						return []string{o.(*fleetv1alpha1.CollectorGroupBinding).Spec.PipelineConfigRef}
					}).
				Build()
			r := &PipelineConfigReconciler{Client: fc, Scheme: fc.Scheme()}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			got := &fleetv1alpha1.PipelineConfig{}
			Expect(fc.Get(ctx, typeNamespacedName, got)).To(Succeed())
			Expect(got.Status.ContentHash).To(BeEmpty(), "the stale hash must be cleared")
			cond := meta.FindStatusCondition(got.Status.Conditions, typeContentValidPipelineConfig)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("EmptyContent"))
		},
		Entry("empty", ""),
		Entry("whitespace", "  \n\t "),
	)

	Context("When deleting a PipelineConfig", func() {
		BeforeEach(func() {
			By("creating the PipelineConfig with the finalizer already set")
			resource := &fleetv1alpha1.PipelineConfig{
				Name:       resourceName,
				Namespace:  namespace,
				Finalizers: []string{PipelineConfigFinalizer},
				Spec: fleetv1alpha1.PipelineConfigSpec{
					Content: "logging { level = \"warn\" }",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(cleanup)

		It("should remove the finalizer and allow deletion when no active bindings exist", func() {
			By("ensuring active bindings is zero")
			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pc)).To(Succeed())
			Expect(pc.Status.ActiveBindings).To(Equal(0))

			By("triggering deletion")
			Expect(k8sClient.Delete(ctx, pc)).To(Succeed())

			By("reconciling the resource under deletion")
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the finalizer was removed")
			updated := &fleetv1alpha1.PipelineConfig{}
			err = k8sClient.Get(ctx, typeNamespacedName, updated)
			if err == nil {
				Expect(updated.Finalizers).NotTo(ContainElement(PipelineConfigFinalizer))
			}
		})
	})
})
