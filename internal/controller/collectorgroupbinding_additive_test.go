// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

var _ = Describe("CollectorGroupBinding additive pipelines and expiry", func() {
	const (
		namespace    = "default"
		validPC      = "additive-pc-valid"
		invalidPC    = "additive-pc-invalid"
		validContent = "local.file \"f\" {\n  filename = \"/tmp/x\"\n}"
	)
	ctx := context.Background()
	start := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := start
	bindingNames := []string{"add-a", "add-b", "col-x", "col.x", "exp-replace", "exp-peer"}

	keyOf := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

	createPC := func(name, content string) {
		pc := &fleetv1alpha1.PipelineConfig{
			Name: name, Namespace: namespace, Spec: fleetv1alpha1.PipelineConfigSpec{Content: content},
		}
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		pc.Status.ContentHash = "hash-" + name
		Expect(k8sClient.Status().Update(ctx, pc)).To(Succeed())
	}

	create := func(name, pc string, mod func(*fleetv1alpha1.CollectorGroupBindingSpec)) {
		spec := fleetv1alpha1.CollectorGroupBindingSpec{TenantRef: "additive-tenant", PipelineConfigRef: pc}
		if mod != nil {
			mod(&spec)
		}
		Expect(k8sClient.Create(ctx, &fleetv1alpha1.CollectorGroupBinding{Name: name, Namespace: namespace, Spec: spec})).To(Succeed())
	}

	additiveMode := func(s *fleetv1alpha1.CollectorGroupBindingSpec) { s.Mode = fleetv1alpha1.BindingModeAdditive }

	cond := func(name, condType string) *metav1.Condition {
		b := &fleetv1alpha1.CollectorGroupBinding{}
		Expect(k8sClient.Get(ctx, keyOf(name), b)).To(Succeed())
		return meta.FindStatusCondition(b.Status.Conditions, condType)
	}

	// reconcile retries: the first pass only adds the finalizer and the cached
	// client can lag behind the API server.
	settle := func(r *CollectorGroupBindingReconciler, name string) (res reconcile.Result) {
		Eventually(func(g Gomega) {
			var err error
			res, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: keyOf(name)})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cond(name, fleetv1alpha1.ConditionTypeRefsValid)).NotTo(BeNil())
		}).Should(Succeed())
		return res
	}

	newReconciler := func() *CollectorGroupBindingReconciler {
		return &CollectorGroupBindingReconciler{
			Client: mgrClient, Scheme: mgrClient.Scheme(), Now: func() time.Time { return now },
		}
	}

	BeforeEach(func() {
		now = start
		createPC(validPC, validContent)
		createPC(invalidPC, "local.file \"f\" {")
	})

	AfterEach(func() {
		for _, n := range bindingNames {
			b := &fleetv1alpha1.CollectorGroupBinding{}
			if err := k8sClient.Get(ctx, keyOf(n), b); err == nil {
				b.Finalizers = nil
				_ = k8sClient.Update(ctx, b)
				_ = k8sClient.Delete(ctx, b)
			}
		}
		for _, n := range []string{validPC, invalidPC} {
			pc := &fleetv1alpha1.PipelineConfig{}
			if err := k8sClient.Get(ctx, keyOf(n), pc); err == nil {
				pc.Finalizers = nil
				_ = k8sClient.Update(ctx, pc)
				_ = k8sClient.Delete(ctx, pc)
			}
		}
	})

	It("sets CompositionValid on additive bindings only", func() {
		create("add-a", validPC, additiveMode)
		create("add-b", invalidPC, additiveMode)
		create("exp-replace", validPC, func(s *fleetv1alpha1.CollectorGroupBindingSpec) { s.TenantRef = "other-tenant" })
		r := newReconciler()
		settle(r, "add-a")
		settle(r, "add-b")
		settle(r, "exp-replace")

		Expect(cond("add-a", fleetv1alpha1.ConditionTypeCompositionValid).Status).To(Equal(metav1.ConditionTrue))
		bad := cond("add-b", fleetv1alpha1.ConditionTypeCompositionValid)
		Expect(bad.Status).To(Equal(metav1.ConditionFalse))
		Expect(bad.Reason).To(Equal("InvalidSyntax"))
		Expect(bad.Message).NotTo(BeEmpty())
		Expect(cond("exp-replace", fleetv1alpha1.ConditionTypeCompositionValid)).To(BeNil())
		Expect(cond("exp-replace", fleetv1alpha1.ConditionTypeNameCollision)).To(BeNil())
	})

	It("flags the later of two additives that derive the same declare label", func() {
		create("col-x", validPC, additiveMode)
		create("col.x", validPC, additiveMode)
		r := newReconciler()
		Eventually(func(g Gomega) {
			g.Expect(mgrClient.Get(ctx, keyOf("col-x"), &fleetv1alpha1.CollectorGroupBinding{})).To(Succeed())
			g.Expect(mgrClient.Get(ctx, keyOf("col.x"), &fleetv1alpha1.CollectorGroupBinding{})).To(Succeed())
		}).Should(Succeed())
		settle(r, "col-x")
		settle(r, "col.x")

		Expect(cond("col-x", fleetv1alpha1.ConditionTypeNameCollision).Status).To(Equal(metav1.ConditionFalse))
		loser := cond("col.x", fleetv1alpha1.ConditionTypeNameCollision)
		Expect(loser.Status).To(Equal(metav1.ConditionTrue))
		Expect(loser.Message).To(ContainSubstring("col-x"))
	})

	It("keeps additive and expired bindings out of the overlap check", func() {
		past := metav1.NewTime(now.Add(-time.Hour))
		create("exp-replace", validPC, func(s *fleetv1alpha1.CollectorGroupBindingSpec) { s.ExpiresAt = &past })
		create("exp-peer", validPC, nil)
		create("add-a", validPC, additiveMode)
		r := newReconciler()
		Eventually(func(g Gomega) {
			for _, n := range []string{"exp-replace", "exp-peer", "add-a"} {
				g.Expect(mgrClient.Get(ctx, keyOf(n), &fleetv1alpha1.CollectorGroupBinding{})).To(Succeed())
			}
		}).Should(Succeed())
		settle(r, "exp-replace")
		settle(r, "exp-peer")
		settle(r, "add-a")

		for _, n := range []string{"exp-replace", "exp-peer", "add-a"} {
			Expect(cond(n, fleetv1alpha1.ConditionTypeOverlap).Status).To(Equal(metav1.ConditionFalse), n)
		}
	})

	It("sets Expired and requeues at expiresAt until it passes, without deleting", func() {
		soon := metav1.NewTime(now.Add(90 * time.Second))
		create("exp-replace", validPC, func(s *fleetv1alpha1.CollectorGroupBindingSpec) { s.ExpiresAt = &soon })
		r := newReconciler()

		res := settle(r, "exp-replace")
		Expect(cond("exp-replace", fleetv1alpha1.ConditionTypeExpired).Status).To(Equal(metav1.ConditionFalse))
		Expect(res.RequeueAfter).To(BeNumerically("~", 90*time.Second, time.Second))

		now = now.Add(2 * time.Minute)
		res = settle(r, "exp-replace")
		Expect(cond("exp-replace", fleetv1alpha1.ConditionTypeExpired).Status).To(Equal(metav1.ConditionTrue))
		Expect(res.RequeueAfter).To(BeZero())
		b := &fleetv1alpha1.CollectorGroupBinding{}
		Expect(k8sClient.Get(ctx, keyOf("exp-replace"), b)).To(Succeed())
		Expect(b.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(b.Status.Phase).To(Equal(fleetv1alpha1.BindingPhaseActive))
	})

	It("never requeues sooner than one second", func() {
		// expiresAt is stored with second precision, so move the clock instead.
		soon := metav1.NewTime(now.Add(time.Second))
		now = now.Add(900 * time.Millisecond)
		create("exp-replace", validPC, func(s *fleetv1alpha1.CollectorGroupBindingSpec) { s.ExpiresAt = &soon })
		res := settle(newReconciler(), "exp-replace")
		Expect(res.RequeueAfter).To(Equal(time.Second))
	})

	It("drops the Expired condition when expiresAt is removed", func() {
		soon := metav1.NewTime(now.Add(time.Hour))
		create("exp-replace", validPC, func(s *fleetv1alpha1.CollectorGroupBindingSpec) { s.ExpiresAt = &soon })
		r := newReconciler()
		settle(r, "exp-replace")
		Expect(cond("exp-replace", fleetv1alpha1.ConditionTypeExpired)).NotTo(BeNil())

		Eventually(func(g Gomega) {
			b := &fleetv1alpha1.CollectorGroupBinding{}
			g.Expect(k8sClient.Get(ctx, keyOf("exp-replace"), b)).To(Succeed())
			b.Spec.ExpiresAt = nil
			g.Expect(k8sClient.Update(ctx, b)).To(Succeed())
		}).Should(Succeed())
		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: keyOf("exp-replace")})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cond("exp-replace", fleetv1alpha1.ConditionTypeExpired)).To(BeNil())
		}).Should(Succeed())
	})
})
