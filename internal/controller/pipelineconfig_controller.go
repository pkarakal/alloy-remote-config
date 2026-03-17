// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Definitions to manage status conditions
const (
	// PipelineConfigFinalizer is the finalizer added to PipelineConfig
	// resources to block deletion while active bindings exist.
	PipelineConfigFinalizer = "fleet.pkarakal.com/pipelineconfig-protection"

	// typeContentValidPipelineConfig is the condition type that indicates
	// whether the PipelineConfig content is non-empty and has been
	// successfully hashed and indexed.
	typeContentValidPipelineConfig = "ContentValid"

	// typeAvailablePipelineConfig is the condition type that indicates
	// the overall availability of the PipelineConfig — set to Unknown
	// on first reconcile, True once fully reconciled.
	// typeAvailablePipelineConfig = "Available"
)

// PipelineConfigReconciler reconciles a PipelineConfig object
type PipelineConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=pipelineconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=pipelineconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=pipelineconfigs/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *PipelineConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Reconciling PipelineConfig")

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
		if pc.Status.ActiveBindings > 0 {
			log.Info("PipelineConfig has active bindings, blocking deletion",
				"activeBindings", pc.Status.ActiveBindings)
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

	// TODO: before shipping v1, add alloy syntax validation
	if pc.Spec.Content == "" {
		meta.SetStatusCondition(&pc.Status.Conditions, metav1.Condition{
			Type:               typeContentValidPipelineConfig,
			Status:             metav1.ConditionFalse,
			Reason:             "EmptyContent",
			Message:            "spec.content must not be empty",
			ObservedGeneration: pc.Generation,
		})
		pc.Status.ObservedGeneration = pc.Generation
		return ctrl.Result{}, r.Status().Update(ctx, pc)
	}

	// compute hash
	sum := sha256.Sum256([]byte(pc.Spec.Content))
	contentDigest := fmt.Sprintf("%x", sum)

	if pc.Status.ContentHash != contentDigest {
		pc.Status.ContentHash = contentDigest
	}

	meta.SetStatusCondition(&pc.Status.Conditions, metav1.Condition{
		Type:               typeContentValidPipelineConfig,
		Status:             metav1.ConditionTrue,
		Reason:             "ContentValid",
		Message:            "PipelineConfig content is valid and indexed",
		ObservedGeneration: pc.Generation,
	})
	pc.Status.ObservedGeneration = pc.Generation

	if err := r.Status().Update(ctx, pc); err != nil {
		log.Error(err, "Failed to update PipelineConfig status")
		return ctrl.Result{}, err
	}

	log.Info("PipelineConfig reconciled successfully")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PipelineConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1alpha1.PipelineConfig{}).
		Named("pipelineconfig").
		Complete(r)
}
