// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/controller"
	"github.com/pkarakal/alloy-remote-config/internal/port"
	"github.com/pkarakal/alloy-remote-config/internal/selection"
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
	log := logf.FromContext(ctx)
	list := &fleetv1alpha1.PipelineConfigList{}
	if err := r.client.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{fleetv1alpha1.LabelDefaultPipelineConfig: "true"},
	); err != nil {
		return nil, fmt.Errorf("listing default PipelineConfigs: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, port.ErrConfigNotFound
	}
	chosen := selection.Pick(list.Items)
	if len(list.Items) > 1 {
		log.Info("Multiple default PipelineConfigs found, using oldest",
			"count", len(list.Items), "chosen", chosen.Name)
	}
	return r.toPipelineResult(chosen)
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

	binding := selection.Pick(list.Items)
	pc := &fleetv1alpha1.PipelineConfig{}
	if err := r.client.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      binding.Spec.PipelineConfigRef,
	}, pc); err != nil {
		if apierrors.IsNotFound(err) {
			// No fallthrough: a dangling binding is a misconfiguration to surface, not skip.
			return nil, fmt.Errorf("PipelineConfig %q referenced by binding %q not found: %w",
				binding.Spec.PipelineConfigRef, binding.Name, port.ErrConfigNotReady)
		}
		return nil, fmt.Errorf("fetching PipelineConfig %q: %w", binding.Spec.PipelineConfigRef, err)
	}

	return r.toPipelineResult(pc)
}

// toPipelineResult converts a PipelineConfig into a ResolvedConfig, returning
// ErrConfigNotReady if the config has not been reconciled yet.
func (r *ConfigResolver) toPipelineResult(pc *fleetv1alpha1.PipelineConfig) (*port.ResolvedConfig, error) {
	// Blank content can predate the CRD rule; never serve it.
	if pc.Status.ContentHash == "" || strings.TrimSpace(pc.Spec.Content) == "" {
		return nil, port.ErrConfigNotReady
	}
	return &port.ResolvedConfig{
		Content:     pc.Spec.Content,
		ContentHash: pc.Status.ContentHash,
	}, nil
}
