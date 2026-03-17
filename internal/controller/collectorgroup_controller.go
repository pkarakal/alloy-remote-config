// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"time"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// CollectorGroupFinalizer is the finalizer added to CollectorGroup
	CollectorGroupFinalizer = "fleet.pkarakal.com/collectorgroup-protection"
)

// CollectorGroupReconciler reconciles a CollectorGroup object
type CollectorGroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=collectorgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=collectorgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=collectorgroups/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *CollectorGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Reconciling CollectorGroup")

	cg := &fleetv1alpha1.CollectorGroup{}
	if err := r.Get(ctx, req.NamespacedName, cg); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("CollectorGroup not found, must have been deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get CollectorGroup")
		return ctrl.Result{}, err
	}

	// handle deletion
	if !cg.DeletionTimestamp.IsZero() {
		if cg.Status.ActiveBindings > 0 {
			log.Info("CollectorGroup has active bindings, blocking deletion",
				"activeBindings", cg.Status.ActiveBindings)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		controllerutil.RemoveFinalizer(cg, CollectorGroupFinalizer)
		return ctrl.Result{}, r.Update(ctx, cg)
	}

	// ensure finalizer
	if !controllerutil.ContainsFinalizer(cg, CollectorGroupFinalizer) {
		controllerutil.AddFinalizer(cg, CollectorGroupFinalizer)
		return ctrl.Result{}, r.Update(ctx, cg)
	}

	cg.Status.ObservedGeneration = cg.Generation
	if err := r.Status().Update(ctx, cg); err != nil {
		log.Error(err, "Failed to update CollectorGroup status")
		return ctrl.Result{}, err
	}

	log.Info("CollectorGroup reconciled successfully")
	return ctrl.Result{}, nil

}

// SetupWithManager sets up the controller with the Manager.
func (r *CollectorGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1alpha1.CollectorGroup{}).
		Named("collectorgroup").
		Complete(r)
}
