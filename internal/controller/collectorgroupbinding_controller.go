// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

const (
	// CollectorGroupBindingFinalizer is the finalizer added to CollectorGroupBinding
	// resources to ensure proper cleanup of counters on deletion.
	CollectorGroupBindingFinalizer = "fleet.pkarakal.com/collectorgroupbinding-protection"

	// IndexTenantRef is the field index key for spec.tenantRef.
	IndexTenantRef = ".spec.tenantRef"
	// IndexCollectorGroupRef is the field index key for spec.collectorGroupRef.
	IndexCollectorGroupRef = ".spec.collectorGroupRef"
	// IndexPipelineConfigRef is the field index key for spec.pipelineConfigRef.
	IndexPipelineConfigRef = ".spec.pipelineConfigRef"
)

var (
	ErrPipelineConfigMissing = errors.New("PipelineConfig referenced by this CollectorGroupBinding does not exist")
	ErrCollectorGroupMissing = errors.New("CollectorGroup referenced by this CollectorGroupBinding does not exist")
)

// CollectorGroupBindingReconciler reconciles a CollectorGroupBinding object
type CollectorGroupBindingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// CollectorTTL is the duration after which a RegisteredTenant entry is
	// evicted if its LastSeenAt has not been refreshed. Zero disables eviction.
	CollectorTTL time.Duration
}

// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=collectorgroupbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=collectorgroupbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=collectorgroupbindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=pipelineconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=pipelineconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=collectorgroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=collectorgroups/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *CollectorGroupBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Reconciling CollectorGroupBinding")

	// Step 0 — Fetch
	groupBinding := &fleetv1alpha1.CollectorGroupBinding{}
	if err := r.Get(ctx, req.NamespacedName, groupBinding); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("CollectorGroupBinding not found, must have been deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get CollectorGroupBinding")
		return ctrl.Result{}, err
	}

	// Step 1 — Deletion path
	if !groupBinding.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.handleDeletion(ctx, groupBinding)
	}

	// Step 2 — Ensure finalizer (return early on first reconcile)
	if !controllerutil.ContainsFinalizer(groupBinding, CollectorGroupBindingFinalizer) {
		controllerutil.AddFinalizer(groupBinding, CollectorGroupBindingFinalizer)
		return ctrl.Result{}, r.Update(ctx, groupBinding)
	}

	// Step 3 — Validate refs
	pipelineConfig, err := r.validateRefs(ctx, groupBinding)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Step 4 — Idempotent counter management
	if err := r.manageCounters(ctx, groupBinding); err != nil {
		return ctrl.Result{}, err
	}

	// Step 4.5 — Evict stale registered tenants
	if evicted := r.evictStaleTenants(groupBinding); evicted > 0 {
		log.Info("Evicted stale registered tenants", "count", evicted, "binding", groupBinding.Name)
	}

	// Step 5 — Update status
	groupBinding.Status.BoundPipelineConfigRef = groupBinding.Spec.PipelineConfigRef
	groupBinding.Status.BoundCollectorGroupRef = groupBinding.Spec.CollectorGroupRef
	groupBinding.Status.ConfigHash = pipelineConfig.Status.ContentHash
	groupBinding.Status.LastSyncedAt = metav1.Now()
	groupBinding.Status.Phase = derivePhase(groupBinding.Status.Conditions)
	groupBinding.Status.ObservedGeneration = groupBinding.Generation

	if err := r.Status().Update(ctx, groupBinding); err != nil {
		return ctrl.Result{}, err
	}
	if r.CollectorTTL > 0 && len(groupBinding.Status.RegisteredTenants) > 0 {
		return ctrl.Result{RequeueAfter: r.CollectorTTL / 2}, nil
	}
	return ctrl.Result{}, nil
}

// evictStaleTenants removes RegisteredTenant entries whose LastSeenAt is older
// than CollectorTTL. Returns the number of evicted entries.
func (r *CollectorGroupBindingReconciler) evictStaleTenants(b *fleetv1alpha1.CollectorGroupBinding) int {
	if r.CollectorTTL == 0 {
		return 0
	}
	cutoff := time.Now().Add(-r.CollectorTTL)
	before := len(b.Status.RegisteredTenants)
	kept := b.Status.RegisteredTenants[:0]
	for _, t := range b.Status.RegisteredTenants {
		if t.LastSeenAt.After(cutoff) {
			kept = append(kept, t)
		}
	}
	b.Status.RegisteredTenants = kept
	return before - len(b.Status.RegisteredTenants)
}

// handleDeletion decrements counters and strips the finalizer.
func (r *CollectorGroupBindingReconciler) handleDeletion(ctx context.Context, b *fleetv1alpha1.CollectorGroupBinding) error {
	log := logf.FromContext(ctx)
	ns := b.Namespace

	if prevPC := b.Status.BoundPipelineConfigRef; prevPC != "" {
		if err := r.decrementPipelineConfigActiveBindings(ctx, ns, prevPC); err != nil && !apierrors.IsNotFound(err) {
			return err
		} else if apierrors.IsNotFound(err) {
			log.Info("PipelineConfig already deleted, skipping decrement", "name", prevPC)
		}
	}

	if prevCG := b.Status.BoundCollectorGroupRef; prevCG != "" {
		if err := r.decrementCollectorGroupActiveBindings(ctx, ns, prevCG); err != nil && !apierrors.IsNotFound(err) {
			return err
		} else if apierrors.IsNotFound(err) {
			log.Info("CollectorGroup already deleted, skipping decrement", "name", prevCG)
		}
	}

	controllerutil.RemoveFinalizer(b, CollectorGroupBindingFinalizer)
	return r.Update(ctx, b)
}

// validateRefs checks that the referenced PipelineConfig and optionally
// CollectorGroup exist. Sets RefsValid condition and updates status on failure.
func (r *CollectorGroupBindingReconciler) validateRefs(
	ctx context.Context, b *fleetv1alpha1.CollectorGroupBinding,
) (*fleetv1alpha1.PipelineConfig, error) {
	ns := b.Namespace

	pipelineConfig, err := r.getPipelineConfig(ctx, ns, b.Spec.PipelineConfigRef)
	if err != nil {
		reason := "PipelineConfigFetchError"
		if apierrors.IsNotFound(err) {
			reason = "PipelineConfigNotFound"
		}
		r.setRefsInvalid(ctx, b, reason, err.Error())
		return nil, err
	}

	if b.Spec.CollectorGroupRef != "" {
		if _, err = r.getCollectorGroup(ctx, ns, b.Spec.CollectorGroupRef); err != nil {
			reason := "CollectorGroupFetchError"
			if apierrors.IsNotFound(err) {
				reason = "CollectorGroupNotFound"
			}
			r.setRefsInvalid(ctx, b, reason, err.Error())
			return nil, err
		}
	}

	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:               fleetv1alpha1.ConditionTypeRefsValid,
		Status:             metav1.ConditionTrue,
		Reason:             "RefsResolved",
		Message:            "All referenced resources exist",
		ObservedGeneration: b.Generation,
	})
	return pipelineConfig, nil
}

// setRefsInvalid sets RefsValid=False and updates status.
func (r *CollectorGroupBindingReconciler) setRefsInvalid(
	ctx context.Context, b *fleetv1alpha1.CollectorGroupBinding,
	reason, msg string,
) {
	log := logf.FromContext(ctx)
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:               fleetv1alpha1.ConditionTypeRefsValid,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: b.Generation,
	})
	b.Status.Phase = derivePhase(b.Status.Conditions)
	b.Status.ObservedGeneration = b.Generation
	if statusErr := r.Status().Update(ctx, b); statusErr != nil {
		log.Error(statusErr, "Failed to update CollectorGroupBinding status")
	}
}

// manageCounters idempotently adjusts PipelineConfig and CollectorGroup
// activeBindings counters when refs change between reconcile cycles.
func (r *CollectorGroupBindingReconciler) manageCounters(
	ctx context.Context, b *fleetv1alpha1.CollectorGroupBinding,
) error {
	log := logf.FromContext(ctx)
	ns := b.Namespace
	prevPC := b.Status.BoundPipelineConfigRef
	newPC := b.Spec.PipelineConfigRef
	prevCG := b.Status.BoundCollectorGroupRef
	newCG := b.Spec.CollectorGroupRef

	if prevPC != "" && prevPC != newPC {
		if err := r.decrementPipelineConfigActiveBindings(ctx, ns, prevPC); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to decrement old PipelineConfig active bindings", "name", prevPC)
		}
	}
	if prevCG != "" && prevCG != newCG {
		if err := r.decrementCollectorGroupActiveBindings(ctx, ns, prevCG); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to decrement old CollectorGroup active bindings", "name", prevCG)
		}
	}

	if prevPC != newPC {
		if err := r.incrementPipelineConfigActiveBindings(ctx, ns, newPC); err != nil {
			return err
		}
	}

	if prevCG != newCG && newCG != "" {
		if err := r.incrementCollectorGroupActiveBindings(ctx, ns, newCG); err != nil {
			return err
		}
	}
	return nil
}

// derivePhase returns Active, Degraded, or Pending based on the RefsValid condition.
func derivePhase(conditions []metav1.Condition) fleetv1alpha1.CollectorGroupBindingPhase {
	refsValid := meta.FindStatusCondition(conditions, fleetv1alpha1.ConditionTypeRefsValid)
	if refsValid == nil {
		return fleetv1alpha1.BindingPhasePending
	}
	if refsValid.Status == metav1.ConditionTrue {
		return fleetv1alpha1.BindingPhaseActive
	}
	return fleetv1alpha1.BindingPhaseDegraded
}

func (r *CollectorGroupBindingReconciler) getPipelineConfig(ctx context.Context, namespace, name string) (*fleetv1alpha1.PipelineConfig, error) {
	log := logf.FromContext(ctx)
	log.Info("Fetching PipelineConfig for CollectorGroupBinding", "namespace", namespace, "name", name)
	pipelineConfig := &fleetv1alpha1.PipelineConfig{}
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pipelineConfig)
	return pipelineConfig, err
}

func (r *CollectorGroupBindingReconciler) getCollectorGroup(ctx context.Context, namespace, name string) (*fleetv1alpha1.CollectorGroup, error) {
	log := logf.FromContext(ctx)
	log.Info("Fetching CollectorGroup for CollectorGroupBinding", "namespace", namespace, "name", name)
	collectorGroup := &fleetv1alpha1.CollectorGroup{}
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, collectorGroup)
	return collectorGroup, err
}

func (r *CollectorGroupBindingReconciler) decrementPipelineConfigActiveBindings(ctx context.Context, namespace, name string) error {
	log := logf.FromContext(ctx)
	log.Info("Decrementing PipelineConfig active bindings", "namespace", namespace, "name", name)
	pipelineConfig, err := r.getPipelineConfig(ctx, namespace, name)
	if err != nil {
		return err
	}
	pipelineConfig.Status.ActiveBindings--
	return r.Status().Update(ctx, pipelineConfig)
}

func (r *CollectorGroupBindingReconciler) incrementPipelineConfigActiveBindings(ctx context.Context, namespace, name string) error {
	log := logf.FromContext(ctx)
	log.Info("Incrementing PipelineConfig active bindings", "namespace", namespace, "name", name)
	pipelineConfig, err := r.getPipelineConfig(ctx, namespace, name)
	if err != nil {
		return err
	}
	pipelineConfig.Status.ActiveBindings++
	return r.Status().Update(ctx, pipelineConfig)
}

func (r *CollectorGroupBindingReconciler) decrementCollectorGroupActiveBindings(ctx context.Context, namespace, name string) error {
	log := logf.FromContext(ctx)
	log.Info("Decrementing CollectorGroup active bindings", "namespace", namespace, "name", name)
	collectorGroup, err := r.getCollectorGroup(ctx, namespace, name)
	if err != nil {
		return err
	}
	collectorGroup.Status.ActiveBindings--
	return r.Status().Update(ctx, collectorGroup)
}

func (r *CollectorGroupBindingReconciler) incrementCollectorGroupActiveBindings(ctx context.Context, namespace, name string) error {
	log := logf.FromContext(ctx)
	log.Info("Incrementing CollectorGroup active bindings", "namespace", namespace, "name", name)
	collectorGroup, err := r.getCollectorGroup(ctx, namespace, name)
	if err != nil {
		return err
	}
	collectorGroup.Status.ActiveBindings++
	return r.Status().Update(ctx, collectorGroup)
}

// enqueueBindingsForPipelineConfig maps a PipelineConfig change to reconcile
// requests for all CollectorGroupBindings referencing it, using the field index.
func (r *CollectorGroupBindingReconciler) enqueueBindingsForPipelineConfig(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	list := &fleetv1alpha1.CollectorGroupBindingList{}
	if err := r.List(ctx, list,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{IndexPipelineConfigRef: obj.GetName()},
	); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, len(list.Items))
	for i, b := range list.Items {
		reqs[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: b.Namespace, Name: b.Name},
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *CollectorGroupBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()

	if err := indexer.IndexField(context.Background(),
		&fleetv1alpha1.CollectorGroupBinding{}, IndexTenantRef,
		func(obj client.Object) []string {
			b := obj.(*fleetv1alpha1.CollectorGroupBinding)
			if b.Spec.TenantRef == "" {
				return nil
			}
			return []string{b.Spec.TenantRef}
		},
	); err != nil {
		return err
	}

	if err := indexer.IndexField(context.Background(),
		&fleetv1alpha1.CollectorGroupBinding{}, IndexCollectorGroupRef,
		func(obj client.Object) []string {
			b := obj.(*fleetv1alpha1.CollectorGroupBinding)
			if b.Spec.CollectorGroupRef == "" {
				return nil
			}
			return []string{b.Spec.CollectorGroupRef}
		},
	); err != nil {
		return err
	}

	if err := indexer.IndexField(context.Background(),
		&fleetv1alpha1.CollectorGroupBinding{}, IndexPipelineConfigRef,
		func(obj client.Object) []string {
			b := obj.(*fleetv1alpha1.CollectorGroupBinding)
			return []string{b.Spec.PipelineConfigRef}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1alpha1.CollectorGroupBinding{}).
		Watches(
			&fleetv1alpha1.PipelineConfig{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueBindingsForPipelineConfig),
		).
		Named("collectorgroupbinding").
		Complete(r)
}
