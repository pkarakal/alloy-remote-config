// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/controller"
	"github.com/pkarakal/alloy-remote-config/internal/metrics"
	"github.com/pkarakal/alloy-remote-config/internal/port"
	"github.com/pkarakal/alloy-remote-config/internal/selection"
)

const (
	// maxPatchRetries bounds optimistic-lock retries. It is sized for bursts of
	// concurrent registrations against one binding, where each round has one winner.
	maxPatchRetries = 12
	// maxRegisteredTenants caps status.registeredTenants per binding to keep the
	// object well below the etcd size limit.
	maxRegisteredTenants = 1000
	// defaultThrottleInterval is used when the collector TTL is disabled (0).
	defaultThrottleInterval = time.Minute
	retryBackoffBase        = 10 * time.Millisecond
	retryBackoffMax         = 250 * time.Millisecond
)

// CollectorRegistry implements port.CollectorRegistry by persisting
// tenant registration state into CollectorGroupBinding status fields.
type CollectorRegistry struct {
	client    client.Client
	reader    client.Reader
	namespace string

	throttleInterval time.Duration
	now              func() time.Time
	metrics          *metrics.RPCMetrics
	// truncatedLogged records bindings already reported as full, so the cap is
	// logged once per binding per process instead of once per poll.
	truncatedLogged sync.Map
}

// RegistryOption customizes a CollectorRegistry.
type RegistryOption func(*CollectorRegistry)

// WithCollectorTTL derives the heartbeat throttle interval (TTL/4) from the
// eviction TTL. A TTL of 0 (eviction disabled) uses a one minute interval.
func WithCollectorTTL(ttl time.Duration) RegistryOption {
	return func(r *CollectorRegistry) { r.throttleInterval = throttleIntervalFor(ttl) }
}

// WithClock overrides the time source, for tests.
func WithClock(now func() time.Time) RegistryOption {
	return func(r *CollectorRegistry) { r.now = now }
}

// WithMetrics enables the registry counters. Nil is allowed.
func WithMetrics(m *metrics.RPCMetrics) RegistryOption {
	return func(r *CollectorRegistry) { r.metrics = m }
}

// throttleIntervalFor returns TTL/4 so a live collector is refreshed several
// times per TTL window; with eviction disabled it falls back to one minute.
func throttleIntervalFor(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return defaultThrottleInterval
	}
	return ttl / 4
}

var _ port.CollectorRegistry = (*CollectorRegistry)(nil)

// NewKubernetesCollectorRegistry creates a registry backed by the given client.
// Typically mgr.GetClient() is passed, which reads from the informer cache and
// has the field indexes registered by CollectorGroupBindingReconciler. reader
// must be an uncached reader (mgr.GetAPIReader()) used to re-read a binding
// after an optimistic-lock conflict, because the cache is what went stale.
func NewKubernetesCollectorRegistry(
	c client.Client, reader client.Reader, namespace string, opts ...RegistryOption,
) *CollectorRegistry {
	r := &CollectorRegistry{
		client:           c,
		reader:           reader,
		namespace:        namespace,
		throttleInterval: defaultThrottleInterval,
		now:              time.Now,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Register upserts the tenant identified by info.LocalAttributes["tenant"] into
// the status.registeredTenants of the matching CollectorGroupBinding, always
// writing. Tenant-specific bindings take priority over group-wide bindings,
// mirroring the config resolution fallback chain.
// If no binding is found, the call is a no-op; collectors may come online
// before their bindings are created.
func (r *CollectorRegistry) Register(ctx context.Context, info port.CollectorInfo) error {
	return r.upsert(ctx, info, false)
}

// Heartbeat refreshes the tenant entry like Register but skips the write when
// the entry was refreshed within the throttle interval.
func (r *CollectorRegistry) Heartbeat(ctx context.Context, info port.CollectorInfo) error {
	return r.upsert(ctx, info, true)
}

func (r *CollectorRegistry) upsert(ctx context.Context, info port.CollectorInfo, throttle bool) error {
	log := logf.FromContext(ctx)

	tenant := info.LocalAttributes["tenant"]
	if tenant == "" {
		// Group-only collectors are valid; there is no tenant entry to record.
		log.V(1).Info("Collector has no tenant attribute, skipping registration",
			"collectorID", info.ID)
		return nil
	}
	collectorGroup := info.LocalAttributes["collector_group"]

	binding, err := r.findBinding(ctx, tenant, collectorGroup)
	if err != nil {
		log.Error(err, "Failed to find binding for tenant", "tenant", tenant)
		return err
	}
	if binding == nil {
		log.V(1).Info("No binding found for tenant, skipping registration",
			"tenant", tenant, "collectorID", info.ID)
		return nil
	}
	key := client.ObjectKeyFromObject(binding)

	for attempt := range maxPatchRetries {
		if attempt > 0 {
			// The cache was stale or another writer won; read live and back off
			// so concurrent writers do not collide in lockstep.
			sleepJitter(ctx, attempt)
			binding = &fleetv1alpha1.CollectorGroupBinding{}
			if err := r.reader.Get(ctx, key, binding); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return fmt.Errorf("re-reading CollectorGroupBinding %q: %w", key.Name, err)
			}
		}

		now := r.now()
		idx := slices.IndexFunc(binding.Status.RegisteredTenants, func(e fleetv1alpha1.RegisteredTenant) bool {
			return e.ID == tenant
		})
		if idx < 0 && len(binding.Status.RegisteredTenants) >= maxRegisteredTenants {
			r.reportTruncated(ctx, binding)
			return nil
		}
		if throttle && idx >= 0 &&
			binding.Status.RegisteredTenants[idx].LastSeenAt.After(now.Add(-r.throttleInterval)) {
			return nil
		}

		base := binding.DeepCopy()
		upsertTenant(&binding.Status.RegisteredTenants, fleetv1alpha1.RegisteredTenant{
			ID:          tenant,
			CollectorID: info.ID,
			LastSeenAt:  metav1.NewTime(now),
		})
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		patchErr := r.client.Status().Patch(ctx, binding, patch)
		if patchErr == nil {
			return nil
		}
		if !apierrors.IsConflict(patchErr) {
			return fmt.Errorf("patching CollectorGroupBinding %q status: %w", binding.Name, patchErr)
		}
		log.V(1).Info("Conflict patching binding status, retrying",
			"attempt", attempt+1, "binding", key.Name)
	}
	return fmt.Errorf("failed to register tenant %q after %d attempts", tenant, maxPatchRetries)
}

// reportTruncated counts a dropped registration and logs once per binding.
func (r *CollectorRegistry) reportTruncated(ctx context.Context, b *fleetv1alpha1.CollectorGroupBinding) {
	if r.metrics != nil && r.metrics.RegistryTruncated != nil {
		r.metrics.RegistryTruncated.Inc()
	}
	if _, loaded := r.truncatedLogged.LoadOrStore(b.Namespace+"/"+b.Name, struct{}{}); !loaded {
		logf.FromContext(ctx).Info("Binding reached registered tenant cap, not registering new tenants",
			"binding", b.Name, "limit", maxRegisteredTenants)
	}
}

// sleepJitter waits a random delay in an exponentially growing, capped window or until ctx is done.
func sleepJitter(ctx context.Context, attempt int) {
	limit := min(int64(retryBackoffBase)<<attempt, int64(retryBackoffMax))
	d := time.Duration(rand.Int64N(limit) + 1) //nolint:gosec // jitter, not security
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
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
		bindingKey := client.ObjectKeyFromObject(&list.Items[i])
		for attempt := range maxPatchRetries {
			if attempt > 0 {
				sleepJitter(ctx, attempt)
			}
			// Always read live: the cached copy is what a conflict proves stale.
			fresh := &fleetv1alpha1.CollectorGroupBinding{}
			if err := r.reader.Get(ctx, bindingKey, fresh); err != nil {
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
			patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
			if patchErr := r.client.Status().Patch(ctx, fresh, patch); patchErr != nil {
				if apierrors.IsConflict(patchErr) {
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
			return selection.Pick(list.Items), nil
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
			return selection.Pick(list.Items), nil
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
