// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"time"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
// +kubebuilder:rbac:groups=fleet.pkarakal.com,resources=collectorgroupbindings,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *CollectorGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("Reconciling CollectorGroup")

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
		active, err := r.countBindings(ctx, cg)
		if err != nil {
			return ctrl.Result{}, err
		}
		if active > 0 {
			log.Info("CollectorGroup has active bindings, blocking deletion",
				"activeBindings", active)
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

	orig := cg.DeepCopy()
	active, err := r.countBindings(ctx, cg)
	if err != nil {
		return ctrl.Result{}, err
	}
	cg.Status.ActiveBindings = active
	cg.Status.ObservedGeneration = cg.Generation
	if !apiequality.Semantic.DeepEqual(orig.Status, cg.Status) {
		if err := r.Status().Update(ctx, cg); err != nil {
			log.Error(err, "Failed to update CollectorGroup status")
			return ctrl.Result{}, err
		}
	}

	log.V(1).Info("CollectorGroup reconciled successfully")
	return ctrl.Result{}, nil
}

// countBindings derives the number of CollectorGroupBindings referencing cg
// from the field index, so the count self-heals after any drift.
func (r *CollectorGroupReconciler) countBindings(ctx context.Context, cg *fleetv1alpha1.CollectorGroup) (int, error) {
	list := &fleetv1alpha1.CollectorGroupBindingList{}
	if err := r.List(ctx, list,
		client.InNamespace(cg.Namespace),
		client.MatchingFields{IndexCollectorGroupRef: cg.Name},
	); err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

// enqueueForBinding maps a binding to the CollectorGroup it references.
func (r *CollectorGroupReconciler) enqueueForBinding(_ context.Context, obj client.Object) []reconcile.Request {
	b, ok := obj.(*fleetv1alpha1.CollectorGroupBinding)
	if !ok || b.Spec.CollectorGroupRef == "" {
		return nil
	}
	return []reconcile.Request{{Namespace: b.Namespace, Name: b.Spec.CollectorGroupRef}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *CollectorGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1alpha1.CollectorGroup{}).
		Watches(&fleetv1alpha1.CollectorGroupBinding{}, handler.EnqueueRequestsFromMapFunc(r.enqueueForBinding)).
		Named("collectorgroup").
		Complete(r)
}
