// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/metrics"
	"github.com/pkarakal/alloy-remote-config/internal/resolution"
	"github.com/pkarakal/alloy-remote-config/internal/selection"
)

const (
	// CollectorGroupBindingFinalizer is the finalizer added to CollectorGroupBinding
	// resources to that existing objects carry; deletion only removes it.
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
	// Metrics holds optional controller-level Prometheus metrics. May be nil.
	Metrics *metrics.ControllerMetrics
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
	log.V(1).Info("Reconciling CollectorGroupBinding")

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

	// Snapshot before later steps mutate status, for Step 5 change detection.
	originalStatus := groupBinding.Status.DeepCopy()

	// Step 3 — Validate refs
	pipelineConfig, err := r.validateRefs(ctx, groupBinding)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Step 3.5 — Flag bindings that lose to a duplicate target
	r.setOverlapCondition(ctx, groupBinding)

	// Step 4.5 — Evict stale registered tenants
	if evicted := r.evictStaleTenants(groupBinding); evicted > 0 {
		log.Info("Evicted stale registered tenants", "count", evicted, "binding", groupBinding.Name)
		if r.Metrics != nil {
			r.Metrics.TenantsEvicted.Add(float64(evicted))
		}
	}

	r.deriveApplyStatus(groupBinding)

	// Step 5 — Update status
	groupBinding.Status.BoundPipelineConfigRef = groupBinding.Spec.PipelineConfigRef
	groupBinding.Status.BoundCollectorGroupRef = groupBinding.Spec.CollectorGroupRef
	groupBinding.Status.ConfigHash = pipelineConfig.Status.ContentHash
	groupBinding.Status.Phase = derivePhase(groupBinding.Status.Conditions)
	groupBinding.Status.ObservedGeneration = groupBinding.Generation
	groupBinding.Status.EffectivePriority = resolution.EffectivePriority(groupBinding)

	// Skip no-op writes: each status write triggers another reconcile and the
	// registry already writes this object on heartbeats. LastSyncedAt must not
	// count as a change or the loop would never settle.
	desired := groupBinding.Status.DeepCopy()
	desired.LastSyncedAt = originalStatus.LastSyncedAt
	if !equality.Semantic.DeepEqual(originalStatus, desired) {
		groupBinding.Status.LastSyncedAt = metav1.Now()
		if err := r.Status().Update(ctx, groupBinding); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.CollectorTTL > 0 && len(groupBinding.Status.RegisteredTenants) > 0 {
		return ctrl.Result{RequeueAfter: r.CollectorTTL / 2}, nil
	}
	return ctrl.Result{}, nil
}

// setOverlapCondition marks the binding as Overlap=True when another binding
// with the same target wins selection, and Overlap=False otherwise. It only
// edits the in-memory conditions; the caller's status write persists them.
func (r *CollectorGroupBindingReconciler) setOverlapCondition(
	ctx context.Context, b *fleetv1alpha1.CollectorGroupBinding,
) {
	log := logf.FromContext(ctx)
	winner, err := r.winningBinding(ctx, b)
	if err != nil {
		log.Error(err, "Failed to list CollectorGroupBindings for overlap check", "binding", b.Name)
		return
	}
	cond := metav1.Condition{
		Type:               fleetv1alpha1.ConditionTypeOverlap,
		Status:             metav1.ConditionFalse,
		Reason:             "UniqueTarget",
		Message:            "No other binding shares this target",
		ObservedGeneration: b.Generation,
	}
	if winner != nil && winner.Name != b.Name {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "DuplicateTarget"
		cond.Message = fmt.Sprintf("Binding %q has the same target and priority, and wins the tie-break", winner.Name)
	}
	meta.SetStatusCondition(&b.Status.Conditions, cond)
}

// winningBinding returns the binding that resolution picks among all bindings
// sharing b's target and effective priority, which is b itself when it is alone.
func (r *CollectorGroupBindingReconciler) winningBinding(
	ctx context.Context, b *fleetv1alpha1.CollectorGroupBinding,
) (*fleetv1alpha1.CollectorGroupBinding, error) {
	indexKey, value := IndexCollectorGroupRef, b.Spec.CollectorGroupRef
	if b.Spec.TenantRef != "" {
		indexKey, value = IndexTenantRef, b.Spec.TenantRef
	}
	list := &fleetv1alpha1.CollectorGroupBindingList{}
	if err := r.List(ctx, list, client.InNamespace(b.Namespace), client.MatchingFields{indexKey: value}); err != nil {
		return nil, err
	}
	// The cache may not hold b yet, so make sure it competes.
	if !slices.ContainsFunc(list.Items, func(o fleetv1alpha1.CollectorGroupBinding) bool { return o.Name == b.Name }) {
		list.Items = append(list.Items, *b)
	}
	prio := resolution.EffectivePriority(b)
	list.Items = slices.DeleteFunc(list.Items, func(o fleetv1alpha1.CollectorGroupBinding) bool {
		return resolution.EffectivePriority(&o) != prio
	})
	return selection.Pick(list.Items), nil
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

// deriveApplyStatus recomputes the apply counts and last error from the
// registered tenants. It runs after eviction so stale entries do not count.
func (r *CollectorGroupBindingReconciler) deriveApplyStatus(b *fleetv1alpha1.CollectorGroupBinding) {
	var applied, failed int32
	var latest *fleetv1alpha1.RegisteredTenant
	for i := range b.Status.RegisteredTenants {
		t := &b.Status.RegisteredTenants[i]
		switch t.ConfigStatus {
		case fleetv1alpha1.ConfigStatusApplied:
			applied++
		case fleetv1alpha1.ConfigStatusFailed:
			failed++
			if latest == nil || changedAfter(t, latest) {
				latest = t
			}
		}
	}
	b.Status.AppliedCount = applied
	b.Status.FailedCount = failed
	b.Status.LastApplyError = ""
	if latest != nil {
		b.Status.LastApplyError = fleetv1alpha1.TruncateError(latest.LastError)
	}
}

// changedAfter reports whether a changed status strictly later than b did; a
// missing timestamp sorts oldest.
func changedAfter(a, b *fleetv1alpha1.RegisteredTenant) bool {
	if a.StatusChangedAt == nil {
		return false
	}
	return b.StatusChangedAt == nil || a.StatusChangedAt.After(b.StatusChangedAt.Time)
}

// handleDeletion strips the finalizer. Counts are derived by the target
// reconcilers, so nothing needs to be unwound here.
func (r *CollectorGroupBindingReconciler) handleDeletion(ctx context.Context, b *fleetv1alpha1.CollectorGroupBinding) error {
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
			Namespace: b.Namespace, Name: b.Name,
		}
	}
	return reqs
}

// enqueueBindingsSharingTarget maps a binding to every binding with the same
// tenantRef or collectorGroupRef, so a loser reacts when the winner changes.
func (r *CollectorGroupBindingReconciler) enqueueBindingsSharingTarget(
	ctx context.Context, b *fleetv1alpha1.CollectorGroupBinding,
) []reconcile.Request {
	var matches []client.ListOption
	if b.Spec.TenantRef != "" {
		matches = append(matches, client.MatchingFields{IndexTenantRef: b.Spec.TenantRef})
	} else if b.Spec.CollectorGroupRef != "" {
		matches = append(matches, client.MatchingFields{IndexCollectorGroupRef: b.Spec.CollectorGroupRef})
	} else {
		return nil
	}
	list := &fleetv1alpha1.CollectorGroupBindingList{}
	if err := r.List(ctx, list, append(matches, client.InNamespace(b.Namespace))...); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, o := range list.Items {
		if o.Name != b.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&o)})
		}
	}
	return reqs
}

// bindingTargetHandler fans out on create, delete and target changes only.
// Reacting to status updates would make every binding re-trigger its peers.
func (r *CollectorGroupBindingReconciler) bindingTargetHandler() handler.EventHandler {
	enqueue := func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request], obj client.Object) {
		if b, ok := obj.(*fleetv1alpha1.CollectorGroupBinding); ok {
			for _, req := range r.enqueueBindingsSharingTarget(ctx, b) {
				q.Add(req)
			}
		}
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, q, e.Object)
		},
		DeleteFunc: func(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, q, e.Object)
		},
		UpdateFunc: func(ctx context.Context, e event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			oldB, okOld := e.ObjectOld.(*fleetv1alpha1.CollectorGroupBinding)
			newB, okNew := e.ObjectNew.(*fleetv1alpha1.CollectorGroupBinding)
			if okOld && okNew && (oldB.Spec.TenantRef != newB.Spec.TenantRef ||
				oldB.Spec.CollectorGroupRef != newB.Spec.CollectorGroupRef ||
				resolution.EffectivePriority(oldB) != resolution.EffectivePriority(newB)) {
				enqueue(ctx, q, oldB)
				enqueue(ctx, q, newB)
			}
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *CollectorGroupBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1alpha1.CollectorGroupBinding{}).
		Watches(
			&fleetv1alpha1.PipelineConfig{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueBindingsForPipelineConfig),
		).
		Watches(&fleetv1alpha1.CollectorGroupBinding{}, r.bindingTargetHandler()).
		Named("collectorgroupbinding").
		Complete(r)
}

// SetupIndexes registers the CollectorGroupBinding field indexes. It must run
// before any controller setup because several reconcilers list by these keys.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()

	if err := indexer.IndexField(ctx,
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

	if err := indexer.IndexField(ctx,
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

	if err := indexer.IndexField(ctx,
		&fleetv1alpha1.CollectorGroupBinding{}, IndexPipelineConfigRef,
		func(obj client.Object) []string {
			b := obj.(*fleetv1alpha1.CollectorGroupBinding)
			return []string{b.Spec.PipelineConfigRef}
		},
	); err != nil {
		return err
	}
	return nil
}
