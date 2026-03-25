// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

var _ = Describe("CollectorGroupBinding Controller", func() {
	const (
		namespace          = "default"
		bindingName        = "test-binding"
		pipelineConfigName = "test-pipelineconfig"
		collectorGroupName = "test-collectorgroup"
		tenantID           = "tenant-abc"
		configContent      = `logging { level = "info" }`
		configHash         = "abc123"
	)

	ctx := context.Background()

	bindingNN := types.NamespacedName{Name: bindingName, Namespace: namespace}
	pcNN := types.NamespacedName{Name: pipelineConfigName, Namespace: namespace}
	cgNN := types.NamespacedName{Name: collectorGroupName, Namespace: namespace}

	newReconciler := func() *CollectorGroupBindingReconciler {
		return &CollectorGroupBindingReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	}

	createPipelineConfig := func(name string) {
		pc := &fleetv1alpha1.PipelineConfig{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pc)
		if apierrors.IsNotFound(err) {
			resource := &fleetv1alpha1.PipelineConfig{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       fleetv1alpha1.PipelineConfigSpec{Content: configContent},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			resource.Status.ContentHash = configHash
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())
		}
	}

	createCollectorGroup := func(name string) {
		cg := &fleetv1alpha1.CollectorGroup{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, cg)
		if apierrors.IsNotFound(err) {
			resource := &fleetv1alpha1.CollectorGroup{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       fleetv1alpha1.CollectorGroupSpec{},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		}
	}

	deleteBinding := func() {
		b := &fleetv1alpha1.CollectorGroupBinding{}
		err := k8sClient.Get(ctx, bindingNN, b)
		if err != nil {
			return
		}
		b.Finalizers = nil
		_ = k8sClient.Update(ctx, b)
		if b.DeletionTimestamp.IsZero() {
			_ = k8sClient.Delete(ctx, b)
		}
	}

	deletePipelineConfig := func(name string) {
		pc := &fleetv1alpha1.PipelineConfig{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pc)
		if err != nil {
			return
		}
		pc.Finalizers = nil
		_ = k8sClient.Update(ctx, pc)
		if pc.DeletionTimestamp.IsZero() {
			_ = k8sClient.Delete(ctx, pc)
		}
	}

	deleteCollectorGroup := func(name string) {
		cg := &fleetv1alpha1.CollectorGroup{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, cg)
		if err != nil {
			return
		}
		if cg.DeletionTimestamp.IsZero() {
			_ = k8sClient.Delete(ctx, cg)
		}
	}

	cleanup := func() {
		deleteBinding()
		deletePipelineConfig(pipelineConfigName)
		deleteCollectorGroup(collectorGroupName)
	}

	Context("When the resource does not exist", func() {
		It("should return no error for a missing CollectorGroupBinding", func() {
			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When reconciling a new CollectorGroupBinding (first reconcile)", func() {
		BeforeEach(func() {
			createPipelineConfig(pipelineConfigName)
			createCollectorGroup(collectorGroupName)
			b := &fleetv1alpha1.CollectorGroupBinding{}
			err := k8sClient.Get(ctx, bindingNN, b)
			if apierrors.IsNotFound(err) {
				resource := &fleetv1alpha1.CollectorGroupBinding{
					ObjectMeta: metav1.ObjectMeta{Name: bindingName, Namespace: namespace},
					Spec: fleetv1alpha1.CollectorGroupBindingSpec{
						PipelineConfigRef: pipelineConfigName,
						CollectorGroupRef: collectorGroupName,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})
		AfterEach(cleanup)

		It("should add the finalizer and return on first reconcile without setting conditions", func() {
			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())

			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, bindingNN, b)).To(Succeed())
			Expect(b.Finalizers).To(ContainElement(CollectorGroupBindingFinalizer))
			Expect(b.Status.Conditions).To(BeEmpty())
		})
	})

	Context("When both refs are valid (CollectorGroupRef)", func() {
		BeforeEach(func() {
			createPipelineConfig(pipelineConfigName)
			createCollectorGroup(collectorGroupName)
			resource := &fleetv1alpha1.CollectorGroupBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:       bindingName,
					Namespace:  namespace,
					Finalizers: []string{CollectorGroupBindingFinalizer},
				},
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					PipelineConfigRef: pipelineConfigName,
					CollectorGroupRef: collectorGroupName,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})
		AfterEach(cleanup)

		It("should set Active phase, RefsValid=True, update counters and status", func() {
			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())

			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, bindingNN, b)).To(Succeed())

			By("checking RefsValid condition")
			cond := meta.FindStatusCondition(b.Status.Conditions, fleetv1alpha1.ConditionTypeRefsValid)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("RefsResolved"))

			By("checking Phase is Active")
			Expect(b.Status.Phase).To(Equal(fleetv1alpha1.BindingPhaseActive))

			By("checking configHash and lastSyncedAt are set")
			Expect(b.Status.ConfigHash).To(Equal(configHash))
			Expect(b.Status.LastSyncedAt.IsZero()).To(BeFalse())

			By("checking BoundPipelineConfigRef and BoundCollectorGroupRef")
			Expect(b.Status.BoundPipelineConfigRef).To(Equal(pipelineConfigName))
			Expect(b.Status.BoundCollectorGroupRef).To(Equal(collectorGroupName))

			By("checking ObservedGeneration")
			Expect(b.Status.ObservedGeneration).To(Equal(b.Generation))

			By("checking PipelineConfig activeBindings incremented")
			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, pcNN, pc)).To(Succeed())
			Expect(pc.Status.ActiveBindings).To(Equal(1))

			By("checking CollectorGroup activeBindings incremented")
			cg := &fleetv1alpha1.CollectorGroup{}
			Expect(k8sClient.Get(ctx, cgNN, cg)).To(Succeed())
			Expect(cg.Status.ActiveBindings).To(Equal(1))
		})
	})

	Context("When the same binding is reconciled again (idempotency)", func() {
		BeforeEach(func() {
			createPipelineConfig(pipelineConfigName)
			createCollectorGroup(collectorGroupName)
			resource := &fleetv1alpha1.CollectorGroupBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:       bindingName,
					Namespace:  namespace,
					Finalizers: []string{CollectorGroupBindingFinalizer},
				},
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					PipelineConfigRef: pipelineConfigName,
					CollectorGroupRef: collectorGroupName,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})
		AfterEach(cleanup)

		It("should not double-increment counters on repeated reconcile", func() {
			r := newReconciler()

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())

			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, pcNN, pc)).To(Succeed())
			Expect(pc.Status.ActiveBindings).To(Equal(1))

			cg := &fleetv1alpha1.CollectorGroup{}
			Expect(k8sClient.Get(ctx, cgNN, cg)).To(Succeed())
			Expect(cg.Status.ActiveBindings).To(Equal(1))
		})
	})

	Context("When TenantRef is used instead of CollectorGroupRef", func() {
		BeforeEach(func() {
			createPipelineConfig(pipelineConfigName)
			resource := &fleetv1alpha1.CollectorGroupBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:       bindingName,
					Namespace:  namespace,
					Finalizers: []string{CollectorGroupBindingFinalizer},
				},
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					PipelineConfigRef: pipelineConfigName,
					TenantRef:         tenantID,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})
		AfterEach(cleanup)

		It("should set Active phase with no CG counter managed", func() {
			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())

			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, bindingNN, b)).To(Succeed())
			Expect(b.Status.Phase).To(Equal(fleetv1alpha1.BindingPhaseActive))

			cond := meta.FindStatusCondition(b.Status.Conditions, fleetv1alpha1.ConditionTypeRefsValid)
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			By("checking BoundCollectorGroupRef is empty (TenantRef does not manage a CG resource)")
			Expect(b.Status.BoundCollectorGroupRef).To(BeEmpty())

			By("checking PipelineConfig activeBindings incremented")
			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, pcNN, pc)).To(Succeed())
			Expect(pc.Status.ActiveBindings).To(Equal(1))
		})
	})

	Context("When PipelineConfig is missing", func() {
		BeforeEach(func() {
			resource := &fleetv1alpha1.CollectorGroupBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:       bindingName,
					Namespace:  namespace,
					Finalizers: []string{CollectorGroupBindingFinalizer},
				},
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					PipelineConfigRef: "nonexistent-pc",
					CollectorGroupRef: collectorGroupName,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})
		AfterEach(cleanup)

		It("should set RefsValid=False and Phase=Degraded", func() {
			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).To(HaveOccurred())

			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, bindingNN, b)).To(Succeed())

			cond := meta.FindStatusCondition(b.Status.Conditions, fleetv1alpha1.ConditionTypeRefsValid)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PipelineConfigNotFound"))
			Expect(b.Status.Phase).To(Equal(fleetv1alpha1.BindingPhaseDegraded))
		})
	})

	Context("When CollectorGroup is missing", func() {
		BeforeEach(func() {
			createPipelineConfig(pipelineConfigName)
			resource := &fleetv1alpha1.CollectorGroupBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:       bindingName,
					Namespace:  namespace,
					Finalizers: []string{CollectorGroupBindingFinalizer},
				},
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					PipelineConfigRef: pipelineConfigName,
					CollectorGroupRef: "nonexistent-cg",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})
		AfterEach(cleanup)

		It("should set RefsValid=False with CollectorGroupNotFound reason", func() {
			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).To(HaveOccurred())

			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, bindingNN, b)).To(Succeed())

			cond := meta.FindStatusCondition(b.Status.Conditions, fleetv1alpha1.ConditionTypeRefsValid)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CollectorGroupNotFound"))
			Expect(b.Status.Phase).To(Equal(fleetv1alpha1.BindingPhaseDegraded))
		})
	})

	Context("When PipelineConfigRef changes", func() {
		const newPCName = "test-pipelineconfig-new"

		BeforeEach(func() {
			createPipelineConfig(pipelineConfigName)
			createPipelineConfig(newPCName)
			createCollectorGroup(collectorGroupName)

			resource := &fleetv1alpha1.CollectorGroupBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:       bindingName,
					Namespace:  namespace,
					Finalizers: []string{CollectorGroupBindingFinalizer},
				},
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					PipelineConfigRef: pipelineConfigName,
					CollectorGroupRef: collectorGroupName,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})
		AfterEach(func() {
			cleanup()
			deletePipelineConfig(newPCName)
		})

		It("should decrement old PC and increment new PC", func() {
			r := newReconciler()

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())

			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, pcNN, pc)).To(Succeed())
			Expect(pc.Status.ActiveBindings).To(Equal(1))

			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, bindingNN, b)).To(Succeed())
			b.Spec.PipelineConfigRef = newPCName
			Expect(k8sClient.Update(ctx, b)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())

			By("checking old PipelineConfig activeBindings decremented")
			Expect(k8sClient.Get(ctx, pcNN, pc)).To(Succeed())
			Expect(pc.Status.ActiveBindings).To(Equal(0))

			By("checking new PipelineConfig activeBindings incremented")
			newPC := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: newPCName, Namespace: namespace}, newPC)).To(Succeed())
			Expect(newPC.Status.ActiveBindings).To(Equal(1))

			By("checking BoundPipelineConfigRef updated")
			Expect(k8sClient.Get(ctx, bindingNN, b)).To(Succeed())
			Expect(b.Status.BoundPipelineConfigRef).To(Equal(newPCName))
		})
	})

	Context("When deleting a CollectorGroupBinding", func() {
		BeforeEach(func() {
			createPipelineConfig(pipelineConfigName)
			createCollectorGroup(collectorGroupName)
			resource := &fleetv1alpha1.CollectorGroupBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:       bindingName,
					Namespace:  namespace,
					Finalizers: []string{CollectorGroupBindingFinalizer},
				},
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					PipelineConfigRef: pipelineConfigName,
					CollectorGroupRef: collectorGroupName,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})
		AfterEach(func() {
			deleteBinding()
			deletePipelineConfig(pipelineConfigName)
			deleteCollectorGroup(collectorGroupName)
		})

		It("should decrement counters and remove finalizer", func() {
			r := newReconciler()

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())

			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, pcNN, pc)).To(Succeed())
			Expect(pc.Status.ActiveBindings).To(Equal(1))

			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, bindingNN, b)).To(Succeed())
			Expect(k8sClient.Delete(ctx, b)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: bindingNN})
			Expect(err).NotTo(HaveOccurred())

			By("checking PipelineConfig activeBindings decremented")
			Expect(k8sClient.Get(ctx, pcNN, pc)).To(Succeed())
			Expect(pc.Status.ActiveBindings).To(Equal(0))

			By("checking CollectorGroup activeBindings decremented")
			cg := &fleetv1alpha1.CollectorGroup{}
			Expect(k8sClient.Get(ctx, cgNN, cg)).To(Succeed())
			Expect(cg.Status.ActiveBindings).To(Equal(0))

			By("checking finalizer removed")
			updated := &fleetv1alpha1.CollectorGroupBinding{}
			err = k8sClient.Get(ctx, bindingNN, updated)
			if err == nil {
				Expect(updated.Finalizers).NotTo(ContainElement(CollectorGroupBindingFinalizer))
			}
		})
	})

	Context("enqueueBindingsForPipelineConfig map function", func() {
		const otherBindingName = "other-binding"
		const otherPCName = "other-pipelineconfig"

		BeforeEach(func() {
			createPipelineConfig(pipelineConfigName)
			createPipelineConfig(otherPCName)
			createCollectorGroup(collectorGroupName)

			b1 := &fleetv1alpha1.CollectorGroupBinding{
				ObjectMeta: metav1.ObjectMeta{Name: bindingName, Namespace: namespace},
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					PipelineConfigRef: pipelineConfigName,
					CollectorGroupRef: collectorGroupName,
				},
			}
			Expect(k8sClient.Create(ctx, b1)).To(Succeed())

			b2 := &fleetv1alpha1.CollectorGroupBinding{
				ObjectMeta: metav1.ObjectMeta{Name: otherBindingName, Namespace: namespace},
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					PipelineConfigRef: otherPCName,
					TenantRef:         tenantID,
				},
			}
			Expect(k8sClient.Create(ctx, b2)).To(Succeed())
		})
		AfterEach(func() {
			b := &fleetv1alpha1.CollectorGroupBinding{}
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: otherBindingName, Namespace: namespace}, b)
			b.Finalizers = nil
			_ = k8sClient.Update(ctx, b)
			_ = k8sClient.Delete(ctx, b)
			cleanup()
			deletePipelineConfig(otherPCName)
		})

		It("should only return requests for bindings referencing the changed PipelineConfig", func() {
			// Use the cache-backed mgrClient so MatchingFields lookups work.
			r := &CollectorGroupBindingReconciler{
				Client: mgrClient,
				Scheme: mgrClient.Scheme(),
			}

			pc := &fleetv1alpha1.PipelineConfig{}
			Expect(k8sClient.Get(ctx, pcNN, pc)).To(Succeed())

			// The cache may lag behind the raw client writes; retry until populated.
			Eventually(func() []reconcile.Request {
				return r.enqueueBindingsForPipelineConfig(ctx, pc)
			}).Should(HaveLen(1))

			reqs := r.enqueueBindingsForPipelineConfig(ctx, pc)
			Expect(reqs[0].NamespacedName).To(Equal(bindingNN))
		})
	})

	Context("TTL eviction of registered tenants", func() {
		const ttlBindingName = "ttl-binding"
		const ttlPCName = "ttl-pc"

		ttlBindingNN := types.NamespacedName{Name: ttlBindingName, Namespace: namespace}

		BeforeEach(func() {
			createPipelineConfig(ttlPCName)
			b := &fleetv1alpha1.CollectorGroupBinding{
				ObjectMeta: metav1.ObjectMeta{Name: ttlBindingName, Namespace: namespace},
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					TenantRef:         "ttl-tenant",
					PipelineConfigRef: ttlPCName,
				},
			}
			Expect(k8sClient.Create(ctx, b)).To(Succeed())
			// Add a finalizer so we control deletion in AfterEach.
			b.Finalizers = []string{CollectorGroupBindingFinalizer}
			Expect(k8sClient.Update(ctx, b)).To(Succeed())
		})

		AfterEach(func() {
			b := &fleetv1alpha1.CollectorGroupBinding{}
			_ = k8sClient.Get(ctx, ttlBindingNN, b)
			b.Finalizers = nil
			_ = k8sClient.Update(ctx, b)
			_ = k8sClient.Delete(ctx, b)
			deletePipelineConfig(ttlPCName)
		})

		seedTenant := func(lastSeenAt metav1.Time) {
			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, ttlBindingNN, b)).To(Succeed())
			b.Status.RegisteredTenants = []fleetv1alpha1.RegisteredTenant{
				{ID: "ttl-tenant", CollectorID: "col-ttl", LastSeenAt: lastSeenAt},
			}
			Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		}

		It("keeps a fresh tenant when TTL has not elapsed", func() {
			seedTenant(metav1.Now())

			r := &CollectorGroupBindingReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				CollectorTTL: time.Minute,
			}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: ttlBindingNN})
			Expect(err).NotTo(HaveOccurred())

			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, ttlBindingNN, b)).To(Succeed())
			Expect(b.Status.RegisteredTenants).To(HaveLen(1))
		})

		It("evicts a stale tenant when TTL has elapsed", func() {
			seedTenant(metav1.NewTime(time.Now().Add(-10 * time.Minute)))

			r := &CollectorGroupBindingReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				CollectorTTL: 5 * time.Minute,
			}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: ttlBindingNN})
			Expect(err).NotTo(HaveOccurred())

			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, ttlBindingNN, b)).To(Succeed())
			Expect(b.Status.RegisteredTenants).To(BeEmpty())
		})

		It("does not evict when CollectorTTL is 0 (disabled)", func() {
			seedTenant(metav1.NewTime(time.Now().Add(-10 * time.Minute)))

			r := &CollectorGroupBindingReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				CollectorTTL: 0,
			}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: ttlBindingNN})
			Expect(err).NotTo(HaveOccurred())

			b := &fleetv1alpha1.CollectorGroupBinding{}
			Expect(k8sClient.Get(ctx, ttlBindingNN, b)).To(Succeed())
			Expect(b.Status.RegisteredTenants).To(HaveLen(1))
		})

		It("returns RequeueAfter=TTL/2 when live tenants exist", func() {
			seedTenant(metav1.Now())

			ttl := 10 * time.Minute
			r := &CollectorGroupBindingReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				CollectorTTL: ttl,
			}
			result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: ttlBindingNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(ttl / 2))
		})
	})
})
