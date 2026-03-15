// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
			Client: k8sClient,
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
				NamespacedName: types.NamespacedName{
					Name:      "nonexistent",
					Namespace: namespace,
				},
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
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: namespace,
					},
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

	Context("When reconciling a PipelineConfig with empty content", func() {
		BeforeEach(func() {
			By("creating a PipelineConfig with empty content")
			resource := &fleetv1alpha1.PipelineConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: fleetv1alpha1.PipelineConfigSpec{
					Content: "",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(cleanup)

		It("should set ContentValid=False and not compute a hash", func() {
			By("adding the finalizer first")
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("reconciling with empty content")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying ContentValid condition is False")
			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pc)).To(Succeed())
			cond := meta.FindStatusCondition(pc.Status.Conditions, typeContentValidPipelineConfig)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("EmptyContent"))

			By("verifying no content hash is set")
			Expect(pc.Status.ContentHash).To(BeEmpty())

			By("verifying ObservedGeneration is set")
			Expect(pc.Status.ObservedGeneration).To(Equal(pc.Generation))
		})
	})

	Context("When deleting a PipelineConfig", func() {
		BeforeEach(func() {
			By("creating the PipelineConfig with the finalizer already set")
			resource := &fleetv1alpha1.PipelineConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:       resourceName,
					Namespace:  namespace,
					Finalizers: []string{PipelineConfigFinalizer},
				},
				Spec: fleetv1alpha1.PipelineConfigSpec{
					Content: "logging { level = \"warn\" }",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(cleanup)

		It("should block deletion and requeue when active bindings exist", func() {
			By("setting active bindings in status")
			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pc)).To(Succeed())
			pc.Status.ActiveBindings = 3
			Expect(k8sClient.Status().Update(ctx, pc)).To(Succeed())

			By("triggering deletion")
			Expect(k8sClient.Delete(ctx, pc)).To(Succeed())

			By("reconciling the resource under deletion")
			reconciler := newReconciler()
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying the finalizer is still present")
			updated := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(PipelineConfigFinalizer))
		})

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
