// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/composition"
	"github.com/pkarakal/alloy-remote-config/internal/validate"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Definitions to manage status conditions
const (
	// PipelineConfigFinalizer is the finalizer added to PipelineConfig
	// resources to block deletion while active bindings exist.
	PipelineConfigFinalizer = "fleet.pkarakal.com/pipelineconfig-protection"

	// typeContentValidPipelineConfig is the condition type that indicates
	// whether the PipelineConfig content is non-empty and has been
	// successfully hashed and indexed.
	typeContentValidPipelineConfig = fleetv1alpha1.ConditionTypeContentValid

	// typeAvailablePipelineConfig is the condition type that indicates
	// the overall availability of the PipelineConfig — set to Unknown
	// on first reconcile, True once fully reconciled.
	// typeAvailablePipelineConfig = "Available"
)

// PipelineConfigReconciler reconciles a PipelineConfig object
type PipelineConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// ContentValidation is the syntax check mode. The zero value means off.
	ContentValidation validate.Mode
}

// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=pipelineconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=pipelineconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=pipelineconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=collectorgroupbindings,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *PipelineConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("Reconciling PipelineConfig")

	pc := &fleetv1alpha1.PipelineConfig{}
	if err := r.Get(ctx, req.NamespacedName, pc); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("PipelineConfig not found, must have been deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get PipelineConfig")
		return ctrl.Result{}, err
	}

	// handle deletion
	if !pc.DeletionTimestamp.IsZero() {
		active, err := r.countBindings(ctx, pc)
		if err != nil {
			return ctrl.Result{}, err
		}
		if active > 0 {
			log.Info("PipelineConfig has active bindings, blocking deletion",
				"activeBindings", active)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		controllerutil.RemoveFinalizer(pc, PipelineConfigFinalizer)
		return ctrl.Result{}, r.Update(ctx, pc)
	}

	// ensure finalizer
	if !controllerutil.ContainsFinalizer(pc, PipelineConfigFinalizer) {
		controllerutil.AddFinalizer(pc, PipelineConfigFinalizer)
		return ctrl.Result{}, r.Update(ctx, pc)
	}

	orig := pc.DeepCopy()
	active, err := r.countBindings(ctx, pc)
	if err != nil {
		return ctrl.Result{}, err
	}
	pc.Status.ActiveBindings = active

	// Blank objects can predate the CRD rule (validation ratcheting keeps them),
	// so drop the hash to make them read as not ready instead of serving it.
	if strings.TrimSpace(pc.Spec.Content) == "" {
		pc.Status.ContentHash = ""
		meta.SetStatusCondition(&pc.Status.Conditions, metav1.Condition{
			Type:               typeContentValidPipelineConfig,
			Status:             metav1.ConditionFalse,
			Reason:             "EmptyContent",
			Message:            "spec.content must not be empty",
			ObservedGeneration: pc.Generation,
		})
		pc.Status.ObservedGeneration = pc.Generation
		return ctrl.Result{}, r.updateStatusIfChanged(ctx, pc, orig)
	}

	// compute hash
	sum := sha256.Sum256([]byte(pc.Spec.Content))
	contentDigest := fmt.Sprintf("%x", sum)

	hadHash := pc.Status.ContentHash != ""
	pc.Status.ContentHash = contentDigest

	if perr := r.applyValidation(pc, contentDigest, hadHash); perr != nil && pc.Status.ObservedGeneration != pc.Generation {
		log.Info("PipelineConfig content did not parse", "mode", r.ContentValidation, "error", perr.Error())
	}
	pc.Status.ObservedGeneration = pc.Generation

	if err := r.updateStatusIfChanged(ctx, pc, orig); err != nil {
		log.Error(err, "Failed to update PipelineConfig status")
		return ctrl.Result{}, err
	}

	log.V(1).Info("PipelineConfig reconciled successfully")
	return ctrl.Result{}, nil
}

// applyValidation sets ContentValid and, in enforce mode, the last-known-good
// fields for non-blank content. hadHash is whether status carried a content
// hash before this reconcile, which marks objects that predate enforce. It
// returns the parse error, if any.
func (r *PipelineConfigReconciler) applyValidation(pc *fleetv1alpha1.PipelineConfig, digest string, hadHash bool) error {
	var perr error
	if r.ContentValidation.Parses() {
		perr = composition.CheckConfig(pc.Spec.Content)
	}
	cond := metav1.Condition{
		Type:               typeContentValidPipelineConfig,
		Status:             metav1.ConditionTrue,
		Reason:             "ContentValid",
		Message:            "PipelineConfig content is valid and indexed",
		ObservedGeneration: pc.Generation,
	}
	if perr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "SyntaxError"
		cond.Message = fleetv1alpha1.TruncateError(perr.Error())
	}
	meta.SetStatusCondition(&pc.Status.Conditions, cond)

	st := &pc.Status
	if r.ContentValidation != validate.ModeEnforce {
		st.LastValidContent, st.LastValidHash, st.ValidatedGeneration = "", "", 0
		meta.RemoveStatusCondition(&st.Conditions, fleetv1alpha1.ConditionTypeSeededUnvalidated)
		meta.RemoveStatusCondition(&st.Conditions, fleetv1alpha1.ConditionTypeContentTooLarge)
		return perr
	}

	// Never-checked objects with a hash were served before enforce existed.
	seeding := st.LastValidContent == "" && st.ValidatedGeneration == 0 && hadHash
	seeded := meta.IsStatusConditionTrue(st.Conditions, fleetv1alpha1.ConditionTypeSeededUnvalidated)
	st.ValidatedGeneration = pc.Generation

	if len(pc.Spec.Content) > fleetv1alpha1.MaxLastValidContentBytes {
		// Copying would risk the etcd object limit; serve it like warn does.
		st.LastValidContent, st.LastValidHash = "", ""
		meta.RemoveStatusCondition(&st.Conditions, fleetv1alpha1.ConditionTypeSeededUnvalidated)
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type:               fleetv1alpha1.ConditionTypeContentTooLarge,
			Status:             metav1.ConditionTrue,
			Reason:             "ContentTooLarge",
			Message:            fmt.Sprintf("Content exceeds %d bytes; last valid content is not kept and the content is served unvalidated", fleetv1alpha1.MaxLastValidContentBytes),
			ObservedGeneration: pc.Generation,
		})
		return perr
	}
	meta.RemoveStatusCondition(&st.Conditions, fleetv1alpha1.ConditionTypeContentTooLarge)

	switch {
	case perr == nil:
		st.LastValidContent, st.LastValidHash = pc.Spec.Content, digest
		meta.RemoveStatusCondition(&st.Conditions, fleetv1alpha1.ConditionTypeSeededUnvalidated)
	case st.LastValidContent == "" && (seeding || seeded):
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type:               fleetv1alpha1.ConditionTypeSeededUnvalidated,
			Status:             metav1.ConditionTrue,
			Reason:             "SeededUnvalidated",
			Message:            "Content predates enforce and does not parse; it is still served until a valid edit replaces it",
			ObservedGeneration: pc.Generation,
		})
	}
	return perr
}

// countBindings derives the number of CollectorGroupBindings referencing pc
// from the field index, so the count self-heals after any drift.
func (r *PipelineConfigReconciler) countBindings(ctx context.Context, pc *fleetv1alpha1.PipelineConfig) (int, error) {
	list := &fleetv1alpha1.CollectorGroupBindingList{}
	if err := r.List(ctx, list,
		client.InNamespace(pc.Namespace),
		client.MatchingFields{IndexPipelineConfigRef: pc.Name},
	); err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

// updateStatusIfChanged skips the API write when status is unchanged.
func (r *PipelineConfigReconciler) updateStatusIfChanged(ctx context.Context, pc, orig *fleetv1alpha1.PipelineConfig) error {
	if apiequality.Semantic.DeepEqual(orig.Status, pc.Status) {
		return nil
	}
	return r.Status().Update(ctx, pc)
}

// enqueueForBinding maps a binding to the PipelineConfig it references.
func (r *PipelineConfigReconciler) enqueueForBinding(_ context.Context, obj client.Object) []reconcile.Request {
	b, ok := obj.(*fleetv1alpha1.CollectorGroupBinding)
	if !ok || b.Spec.PipelineConfigRef == "" {
		return nil
	}
	return []reconcile.Request{{Namespace: b.Namespace, Name: b.Spec.PipelineConfigRef}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *PipelineConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1alpha1.PipelineConfig{}).
		Watches(&fleetv1alpha1.CollectorGroupBinding{}, handler.EnqueueRequestsFromMapFunc(r.enqueueForBinding)).
		Named("pipelineconfig").
		Complete(r)
}
