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
	"github.com/pkarakal/alloy-remote-config/internal/port"
	"github.com/pkarakal/alloy-remote-config/internal/resolution"
	"github.com/pkarakal/alloy-remote-config/internal/selection"
)

// ConfigResolver implements port.ConfigResolver using the controller-runtime
// informer cache. It relies on field indexes registered by the
// CollectorGroupBinding controller.
type ConfigResolver struct {
	client   client.Reader
	selector *bindingSelector
}

var _ port.ConfigResolver = (*ConfigResolver)(nil)

// NewConfigResolver creates a ConfigResolver backed by the given client.Reader
// (typically mgr.GetClient(), which reads from the informer cache).
func NewConfigResolver(c client.Reader) *ConfigResolver {
	return &ConfigResolver{client: c, selector: newBindingSelector(c)}
}

// Resolve ranks the tenant bindings, the bindings of every group the collector
// belongs to, and the default config; the winner decides.
func (r *ConfigResolver) Resolve(ctx context.Context, namespace string, attrs map[string]string) (*port.ResolvedConfig, error) {
	var defaults []fleetv1alpha1.PipelineConfig
	decision, err := r.selector.decide(ctx, namespace, attrs, func(ctx context.Context) (bool, error) {
		var err error
		defaults, err = r.listDefaults(ctx, namespace)
		return len(defaults) > 0, err
	})
	if err != nil {
		return nil, err
	}

	switch decision.Kind {
	case resolution.KindTenant, resolution.KindGroup:
		return r.resolveBinding(ctx, namespace, decision)
	case resolution.KindDefault:
		chosen := selection.Pick(defaults)
		if len(defaults) > 1 {
			logf.FromContext(ctx).Info("Multiple default PipelineConfigs found, using oldest",
				"count", len(defaults), "chosen", chosen.Name)
		}
		resolved, err := r.toPipelineResult(chosen)
		if err != nil {
			return nil, &port.ResolveError{Path: port.PathDefault, Err: err}
		}
		resolved.Path = port.PathDefault
		return resolved, nil
	default:
		return nil, &port.ResolveError{Path: port.PathDefault, Err: port.ErrConfigNotFound}
	}
}

func (r *ConfigResolver) listDefaults(ctx context.Context, namespace string) ([]fleetv1alpha1.PipelineConfig, error) {
	list := &fleetv1alpha1.PipelineConfigList{}
	if err := r.client.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{fleetv1alpha1.LabelDefaultPipelineConfig: "true"},
	); err != nil {
		return nil, fmt.Errorf("listing default PipelineConfigs: %w", err)
	}
	return list.Items, nil
}

// resolveBinding returns the PipelineConfig content referenced by the winning binding.
func (r *ConfigResolver) resolveBinding(ctx context.Context, namespace string, d resolution.Decision) (*port.ResolvedConfig, error) {
	path := port.PathCollectorGroup
	if d.Kind == resolution.KindTenant {
		path = port.PathTenant
	}
	fail := func(err error) (*port.ResolvedConfig, error) {
		return nil, &port.ResolveError{Path: path, BindingName: d.Binding.Name, Err: err}
	}

	pc := &fleetv1alpha1.PipelineConfig{}
	if err := r.client.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      d.Binding.Spec.PipelineConfigRef,
	}, pc); err != nil {
		if apierrors.IsNotFound(err) {
			// No fallthrough: a dangling binding is a misconfiguration to surface, not skip.
			return fail(fmt.Errorf("PipelineConfig %q referenced by binding %q not found: %w",
				d.Binding.Spec.PipelineConfigRef, d.Binding.Name, port.ErrConfigNotReady))
		}
		return fail(fmt.Errorf("fetching PipelineConfig %q: %w", d.Binding.Spec.PipelineConfigRef, err))
	}

	resolved, err := r.toPipelineResult(pc)
	if err != nil {
		return fail(err)
	}
	resolved.Path = path
	resolved.BindingName = d.Binding.Name
	return resolved, nil
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
