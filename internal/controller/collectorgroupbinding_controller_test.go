// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

var _ = Describe("CollectorGroupBinding Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		const collectorGroupName = "collectorgroup-sample"
		const pipelineConfigName = "pipelineconfig-sample"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		collectorgroupbinding := &fleetv1alpha1.CollectorGroupBinding{
			Spec: fleetv1alpha1.CollectorGroupBindingSpec{
				CollectorGroupRef: collectorGroupName,
				PipelineConfigRef: pipelineConfigName,
			},
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind CollectorGroupBinding")
			err := k8sClient.Get(ctx, typeNamespacedName, collectorgroupbinding)
			if err != nil && errors.IsNotFound(err) {
				resource := &fleetv1alpha1.CollectorGroupBinding{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: fleetv1alpha1.CollectorGroupBindingSpec{
						CollectorGroupRef: collectorGroupName,
						PipelineConfigRef: pipelineConfigName,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &fleetv1alpha1.CollectorGroupBinding{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance CollectorGroupBinding")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &CollectorGroupBindingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
