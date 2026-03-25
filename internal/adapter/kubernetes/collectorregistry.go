// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/controller"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

const maxPatchRetries = 3

// CollectorRegistry implements port.CollectorRegistry by persisting
// tenant registration state into CollectorGroupBinding status fields.
type CollectorRegistry struct {
	client    client.Client
	namespace string
}

var _ port.CollectorRegistry = (*CollectorRegistry)(nil)

// NewKubernetesCollectorRegistry creates a registry backed by the given client.
// Typically mgr.GetClient() is passed, which reads from the informer cache and
// has the field indexes registered by CollectorGroupBindingReconciler.
func NewKubernetesCollectorRegistry(c client.Client, namespace string) *CollectorRegistry {
	return &CollectorRegistry{client: c, namespace: namespace}
}

// Register upserts the tenant identified by info.LocalAttributes["tenant"] into
// the status.registeredTenants of the matching CollectorGroupBinding.
// Tenant-specific bindings take priority over group-wide bindings, mirroring
// the config resolution fallback chain.
// If no binding is found, the call is a no-op; collectors may come online
// before their bindings are created.
func (r *CollectorRegistry) Register(ctx context.Context, info port.CollectorInfo) error {
	log := logf.FromContext(ctx)

	tenant := info.LocalAttributes["tenant"]
	if tenant == "" {
		return fmt.Errorf("collector %q has no tenant attribute; skipping registration", info.ID)
	}
	collectorGroup := info.LocalAttributes["collector_group"]

	for attempt := range maxPatchRetries {
		binding, err := r.findBinding(ctx, tenant, collectorGroup)
		if err != nil {
			log.Error(err, "error finding binding for tenant", "tenant", tenant)
			return err
		}
		if binding == nil {
			log.V(1).Info("No binding found for tenant, skipping registration",
				"tenant", tenant, "collectorID", info.ID)
			return nil
		}

		base := binding.DeepCopy()
		upsertTenant(&binding.Status.RegisteredTenants, fleetv1alpha1.RegisteredTenant{
			ID:          tenant,
			CollectorID: info.ID,
			LastSeenAt:  metav1.Now(),
		})
		if patchErr := r.client.Status().Patch(ctx, binding, client.MergeFrom(base)); patchErr != nil {
			if apierrors.IsConflict(patchErr) && attempt < maxPatchRetries-1 {
				log.V(1).Info("Conflict patching binding status, retrying",
					"attempt", attempt+1, "binding", binding.Name)
				continue
			}
			return fmt.Errorf("patching CollectorGroupBinding %q status: %w", binding.Name, patchErr)
		}
		return nil
	}
	return fmt.Errorf("failed to register tenant %q after %d attempts", tenant, maxPatchRetries)
}

// Unregister removes the entry whose CollectorID matches id from whichever
// CollectorGroupBinding holds it. The call is idempotent: if id is not found
// in any binding, it returns nil.
func (r *CollectorRegistry) Unregister(ctx context.Context, id string) error {
	log := logf.FromContext(ctx)

	list := &fleetv1alpha1.CollectorGroupBindingList{}
	if err := r.client.List(ctx, list, client.InNamespace(r.namespace)); err != nil {
		return fmt.Errorf("listing CollectorGroupBindings: %w", err)
	}

	for i := range list.Items {
		if indexTenantByCollectorID(list.Items[i].Status.RegisteredTenants, id) < 0 {
			continue
		}
		// Found, re-fetch and patch to avoid stale resourceVersion.
		bindingKey := client.ObjectKeyFromObject(&list.Items[i])
		for attempt := range maxPatchRetries {
			fresh := &fleetv1alpha1.CollectorGroupBinding{}
			if err := r.client.Get(ctx, bindingKey, fresh); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return fmt.Errorf("re-fetching CollectorGroupBinding %q: %w", bindingKey.Name, err)
			}
			idx := indexTenantByCollectorID(fresh.Status.RegisteredTenants, id)
			if idx < 0 {
				// Already removed by a concurrent call.
				return nil
			}
			base := fresh.DeepCopy()
			fresh.Status.RegisteredTenants = slices.Delete(fresh.Status.RegisteredTenants, idx, idx+1)
			if patchErr := r.client.Status().Patch(ctx, fresh, client.MergeFrom(base)); patchErr != nil {
				if apierrors.IsConflict(patchErr) && attempt < maxPatchRetries-1 {
					log.V(1).Info("Conflict unregistering collector, retrying",
						"collectorID", id, "binding", fresh.Name, "attempt", attempt+1)
					continue
				}
				return fmt.Errorf("patching CollectorGroupBinding %q: %w", fresh.Name, patchErr)
			}
			log.Info("Unregistered collector from binding",
				"collectorID", id, "binding", fresh.Name)
			return nil
		}
		return fmt.Errorf("failed to unregister collector %q after %d attempts", id, maxPatchRetries)
	}

	log.V(1).Info("Collector not found in any binding during unregister", "collectorID", id)
	return nil
}

// findBinding returns the CollectorGroupBinding that should receive the tenant's
// registration. Tenant-specific bindings take priority over group-wide bindings.
func (r *CollectorRegistry) findBinding(
	ctx context.Context, tenant, collectorGroup string,
) (*fleetv1alpha1.CollectorGroupBinding, error) {
	if tenant != "" {
		list := &fleetv1alpha1.CollectorGroupBindingList{}
		if err := r.client.List(ctx, list,
			client.InNamespace(r.namespace),
			client.MatchingFields{controller.IndexTenantRef: tenant},
		); err != nil {
			return nil, fmt.Errorf("listing tenant bindings: %w", err)
		}
		if len(list.Items) > 0 {
			return &list.Items[0], nil
		}
	}
	if collectorGroup != "" {
		list := &fleetv1alpha1.CollectorGroupBindingList{}
		if err := r.client.List(ctx, list,
			client.InNamespace(r.namespace),
			client.MatchingFields{controller.IndexCollectorGroupRef: collectorGroup},
		); err != nil {
			return nil, fmt.Errorf("listing group bindings: %w", err)
		}
		if len(list.Items) > 0 {
			return &list.Items[0], nil
		}
	}
	return nil, nil
}

// upsertTenant updates an existing entry matching t.ID or appends a new one.
func upsertTenant(tenants *[]fleetv1alpha1.RegisteredTenant, t fleetv1alpha1.RegisteredTenant) {
	if idx := slices.IndexFunc(*tenants, func(e fleetv1alpha1.RegisteredTenant) bool {
		return e.ID == t.ID
	}); idx >= 0 {
		(*tenants)[idx] = t
		return
	}
	*tenants = append(*tenants, t)
}

// indexTenantByCollectorID returns the index of the entry with the given
// collectorID, or -1 if not found.
func indexTenantByCollectorID(tenants []fleetv1alpha1.RegisteredTenant, collectorID string) int {
	return slices.IndexFunc(tenants, func(t fleetv1alpha1.RegisteredTenant) bool {
		return t.CollectorID == collectorID
	})
}
