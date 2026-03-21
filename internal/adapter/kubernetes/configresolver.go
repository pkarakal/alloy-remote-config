// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/controller"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

// ConfigResolver implements port.ConfigResolver using the controller-runtime
// informer cache. It relies on field indexes registered by the
// CollectorGroupBinding controller.
type ConfigResolver struct {
	client client.Reader
}

var _ port.ConfigResolver = (*ConfigResolver)(nil)

// NewConfigResolver creates a ConfigResolver backed by the given client.Reader
// (typically mgr.GetClient(), which reads from the informer cache).
func NewConfigResolver(c client.Reader) *ConfigResolver {
	return &ConfigResolver{client: c}
}

func (r *ConfigResolver) ResolveByTenant(ctx context.Context, namespace, tenant string) (*port.ResolvedConfig, error) {
	return r.resolveByIndex(ctx, namespace, controller.IndexTenantRef, tenant)
}

func (r *ConfigResolver) ResolveByCollectorGroup(ctx context.Context, namespace, collectorGroup string) (*port.ResolvedConfig, error) {
	return r.resolveByIndex(ctx, namespace, controller.IndexCollectorGroupRef, collectorGroup)
}

func (r *ConfigResolver) ResolveDefault(ctx context.Context, namespace string) (*port.ResolvedConfig, error) {
	pc := &fleetv1alpha1.PipelineConfig{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "default"}, pc); err != nil {
		return nil, port.ErrConfigNotFound
	}
	return r.toPipelineResult(pc)
}

// resolveByIndex lists CollectorGroupBindings matching the given field index
// and returns the referenced PipelineConfig content.
func (r *ConfigResolver) resolveByIndex(ctx context.Context, namespace, indexKey, indexValue string) (*port.ResolvedConfig, error) {
	list := &fleetv1alpha1.CollectorGroupBindingList{}
	if err := r.client.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingFields{indexKey: indexValue},
	); err != nil {
		return nil, fmt.Errorf("listing CollectorGroupBindings by %s: %w", indexKey, err)
	}

	if len(list.Items) == 0 {
		return nil, port.ErrConfigNotFound
	}

	// Use the first matching binding's PipelineConfig reference.
	binding := &list.Items[0]
	pc := &fleetv1alpha1.PipelineConfig{}
	if err := r.client.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      binding.Spec.PipelineConfigRef,
	}, pc); err != nil {
		return nil, fmt.Errorf("fetching PipelineConfig %q: %w", binding.Spec.PipelineConfigRef, err)
	}

	return r.toPipelineResult(pc)
}

// toPipelineResult converts a PipelineConfig into a ResolvedConfig, returning
// ErrConfigNotReady if the config has not been reconciled yet.
func (r *ConfigResolver) toPipelineResult(pc *fleetv1alpha1.PipelineConfig) (*port.ResolvedConfig, error) {
	if pc.Status.ContentHash == "" {
		return nil, port.ErrConfigNotReady
	}
	return &port.ResolvedConfig{
		Content:     pc.Spec.Content,
		ContentHash: pc.Status.ContentHash,
	}, nil
}
