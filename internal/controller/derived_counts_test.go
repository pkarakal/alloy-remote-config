// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

// indexedListClient writes through the raw client but lists from the
// index-backed cache, which is what the reconcilers get from the manager.
type indexedListClient struct {
	client.Client
	lister client.Reader
}

func (c indexedListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.lister.List(ctx, list, opts...)
}

var _ = Describe("Derived activeBindings", func() {
	const namespace = "default"

	ctx := context.Background()

	var (
		pcR *PipelineConfigReconciler
		cgR *CollectorGroupReconciler
	)

	nn := func(name string) types.NamespacedName {
		return types.NamespacedName{Namespace: namespace, Name: name}
	}

	// reconcileBoth runs both reconcilers, standing in for the controllers
	// that are not started in this suite.
	reconcileBoth := func(pcName, cgName string) {
		if pcName != "" {
			_, err := pcR.Reconcile(ctx, reconcile.Request{NamespacedName: nn(pcName)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
		if cgName != "" {
			_, err := cgR.Reconcile(ctx, reconcile.Request{NamespacedName: nn(cgName)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
	}

	pcCount := func(name string) func() (int, error) {
		return func() (int, error) {
			reconcileBoth(name, "")
			pc := &fleetv1alpha1.PipelineConfig{}
			if err := k8sClient.Get(ctx, nn(name), pc); err != nil {
				return -1, err
			}
			return pc.Status.ActiveBindings, nil
		}
	}

	cgCount := func(name string) func() (int, error) {
		return func() (int, error) {
			reconcileBoth("", name)
			cg := &fleetv1alpha1.CollectorGroup{}
			if err := k8sClient.Get(ctx, nn(name), cg); err != nil {
				return -1, err
			}
			return cg.Status.ActiveBindings, nil
		}
	}

	createPC := func(name string) {
		Expect(k8sClient.Create(ctx, &fleetv1alpha1.PipelineConfig{
			Name: name, Namespace: namespace,
			Spec: fleetv1alpha1.PipelineConfigSpec{Content: `logging { level = "info" }`},
		})).To(Succeed())
		// First reconcile only adds the finalizer.
		reconcileBoth(name, "")
	}

	createCG := func(name string) {
		Expect(k8sClient.Create(ctx, &fleetv1alpha1.CollectorGroup{
			Name: name, Namespace: namespace,
		})).To(Succeed())
		reconcileBoth("", name)
	}

	createBinding := func(name string, spec fleetv1alpha1.CollectorGroupBindingSpec) {
		Expect(k8sClient.Create(ctx, &fleetv1alpha1.CollectorGroupBinding{
			Name: name, Namespace: namespace, Spec: spec,
		})).To(Succeed())
	}

	deleteBinding := func(name string) {
		b := &fleetv1alpha1.CollectorGroupBinding{}
		Expect(k8sClient.Get(ctx, nn(name), b)).To(Succeed())
		Expect(k8sClient.Delete(ctx, b)).To(Succeed())
	}

	// forceDelete strips finalizers so leftovers never leak across specs.
	forceDelete := func(obj client.Object) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return
		}
		obj.SetFinalizers(nil)
		_ = k8sClient.Update(ctx, obj)
		_ = k8sClient.Delete(ctx, obj)
	}

	names := struct{ pcA, pcB, cgA, cgB, b1, b2 string }{"dc-pc-a", "dc-pc-b", "dc-cg-a", "dc-cg-b", "dc-binding-1", "dc-binding-2"}

	BeforeEach(func() {
		lister := indexedListClient{Client: k8sClient, lister: mgrClient}
		pcR = &PipelineConfigReconciler{Client: lister, Scheme: k8sClient.Scheme()}
		cgR = &CollectorGroupReconciler{Client: lister, Scheme: k8sClient.Scheme()}
		createPC(names.pcA)
		createPC(names.pcB)
		createCG(names.cgA)
		createCG(names.cgB)
	})

	AfterEach(func() {
		for _, n := range []string{names.b1, names.b2} {
			forceDelete(&fleetv1alpha1.CollectorGroupBinding{Name: n, Namespace: namespace})
		}
		forceDelete(&fleetv1alpha1.PipelineConfig{Name: names.pcA, Namespace: namespace})
		forceDelete(&fleetv1alpha1.PipelineConfig{Name: names.pcB, Namespace: namespace})
		forceDelete(&fleetv1alpha1.CollectorGroup{Name: names.cgA, Namespace: namespace})
		forceDelete(&fleetv1alpha1.CollectorGroup{Name: names.cgB, Namespace: namespace})
	})

	It("moves the count when a binding changes its pipelineConfigRef from A to B", func() {
		createBinding(names.b1, fleetv1alpha1.CollectorGroupBindingSpec{PipelineConfigRef: names.pcA, CollectorGroupRef: names.cgA})
		Eventually(pcCount(names.pcA), 10*time.Second, 100*time.Millisecond).Should(Equal(1))

		b := &fleetv1alpha1.CollectorGroupBinding{}
		Expect(k8sClient.Get(ctx, nn(names.b1), b)).To(Succeed())
		b.Spec.PipelineConfigRef = names.pcB
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		Eventually(pcCount(names.pcA), 10*time.Second, 100*time.Millisecond).Should(Equal(0))
		Eventually(pcCount(names.pcB), 10*time.Second, 100*time.Millisecond).Should(Equal(1))
	})

	It("moves the count when a binding changes its collectorGroupRef from A to B", func() {
		createBinding(names.b1, fleetv1alpha1.CollectorGroupBindingSpec{PipelineConfigRef: names.pcA, CollectorGroupRef: names.cgA})
		Eventually(cgCount(names.cgA), 10*time.Second, 100*time.Millisecond).Should(Equal(1))

		b := &fleetv1alpha1.CollectorGroupBinding{}
		Expect(k8sClient.Get(ctx, nn(names.b1), b)).To(Succeed())
		b.Spec.CollectorGroupRef = names.cgB
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		Eventually(cgCount(names.cgA), 10*time.Second, 100*time.Millisecond).Should(Equal(0))
		Eventually(cgCount(names.cgB), 10*time.Second, 100*time.Millisecond).Should(Equal(1))
	})

	It("heals a drifted PipelineConfig count on the next reconcile", func() {
		createBinding(names.b1, fleetv1alpha1.CollectorGroupBindingSpec{PipelineConfigRef: names.pcA, CollectorGroupRef: names.cgA})
		Eventually(pcCount(names.pcA), 10*time.Second, 100*time.Millisecond).Should(Equal(1))

		pc := &fleetv1alpha1.PipelineConfig{}
		Expect(k8sClient.Get(ctx, nn(names.pcA), pc)).To(Succeed())
		pc.Status.ActiveBindings = 5
		Expect(k8sClient.Status().Update(ctx, pc)).To(Succeed())

		Expect(k8sClient.Get(ctx, nn(names.pcA), pc)).To(Succeed())
		pc.Annotations = map[string]string{"test/poke": "1"}
		Expect(k8sClient.Update(ctx, pc)).To(Succeed())

		Eventually(pcCount(names.pcA), 10*time.Second, 100*time.Millisecond).Should(Equal(1))
	})

	It("heals a drifted CollectorGroup count on the next reconcile", func() {
		createBinding(names.b1, fleetv1alpha1.CollectorGroupBindingSpec{PipelineConfigRef: names.pcA, CollectorGroupRef: names.cgA})
		Eventually(cgCount(names.cgA), 10*time.Second, 100*time.Millisecond).Should(Equal(1))

		cg := &fleetv1alpha1.CollectorGroup{}
		Expect(k8sClient.Get(ctx, nn(names.cgA), cg)).To(Succeed())
		cg.Status.ActiveBindings = 5
		Expect(k8sClient.Status().Update(ctx, cg)).To(Succeed())

		Eventually(cgCount(names.cgA), 10*time.Second, 100*time.Millisecond).Should(Equal(1))
	})

	It("blocks PipelineConfig deletion while a binding references it", func() {
		createBinding(names.b1, fleetv1alpha1.CollectorGroupBindingSpec{PipelineConfigRef: names.pcA, CollectorGroupRef: names.cgA})
		Eventually(pcCount(names.pcA), 10*time.Second, 100*time.Millisecond).Should(Equal(1))

		pc := &fleetv1alpha1.PipelineConfig{}
		Expect(k8sClient.Get(ctx, nn(names.pcA), pc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pc)).To(Succeed())

		res, err := pcR.Reconcile(ctx, reconcile.Request{NamespacedName: nn(names.pcA)})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(30 * time.Second))
		Expect(k8sClient.Get(ctx, nn(names.pcA), pc)).To(Succeed())
		Expect(pc.Finalizers).To(ContainElement(PipelineConfigFinalizer))

		deleteBinding(names.b1)
		Eventually(func() bool {
			_, err := pcR.Reconcile(ctx, reconcile.Request{NamespacedName: nn(names.pcA)})
			Expect(err).NotTo(HaveOccurred())
			return apierrors.IsNotFound(k8sClient.Get(ctx, nn(names.pcA), &fleetv1alpha1.PipelineConfig{}))
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
	})

	It("blocks CollectorGroup deletion while a binding references it", func() {
		createBinding(names.b1, fleetv1alpha1.CollectorGroupBindingSpec{PipelineConfigRef: names.pcA, CollectorGroupRef: names.cgA})
		Eventually(cgCount(names.cgA), 10*time.Second, 100*time.Millisecond).Should(Equal(1))

		cg := &fleetv1alpha1.CollectorGroup{}
		Expect(k8sClient.Get(ctx, nn(names.cgA), cg)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cg)).To(Succeed())

		res, err := cgR.Reconcile(ctx, reconcile.Request{NamespacedName: nn(names.cgA)})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(30 * time.Second))
		Expect(k8sClient.Get(ctx, nn(names.cgA), cg)).To(Succeed())
		Expect(cg.Finalizers).To(ContainElement(CollectorGroupFinalizer))

		deleteBinding(names.b1)
		Eventually(func() bool {
			_, err := cgR.Reconcile(ctx, reconcile.Request{NamespacedName: nn(names.cgA)})
			Expect(err).NotTo(HaveOccurred())
			return apierrors.IsNotFound(k8sClient.Get(ctx, nn(names.cgA), &fleetv1alpha1.CollectorGroup{}))
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
	})

	It("does not count tenant bindings toward any group", func() {
		createBinding(names.b1, fleetv1alpha1.CollectorGroupBindingSpec{PipelineConfigRef: names.pcA, TenantRef: "tenant-x"})
		Eventually(pcCount(names.pcA), 10*time.Second, 100*time.Millisecond).Should(Equal(1))
		Consistently(cgCount(names.cgA), 1*time.Second, 200*time.Millisecond).Should(Equal(0))
		Consistently(cgCount(names.cgB), 1*time.Second, 200*time.Millisecond).Should(Equal(0))
	})
})
