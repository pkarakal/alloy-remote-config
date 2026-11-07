// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/composition"
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

// ResolverOption customizes a ConfigResolver.
type ResolverOption func(*ConfigResolver)

// WithResolverClock overrides the expiry clock, for tests.
func WithResolverClock(now func() time.Time) ResolverOption {
	return func(r *ConfigResolver) { r.selector.now = now }
}

// NewConfigResolver creates a ConfigResolver backed by the given client.Reader
// (typically mgr.GetClient(), which reads from the informer cache).
func NewConfigResolver(c client.Reader, opts ...ResolverOption) *ConfigResolver {
	r := &ConfigResolver{client: c, selector: newBindingSelector(c)}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Resolve ranks the tenant bindings, the bindings of every group the collector
// belongs to, and the default config; the winner decides. Additive bindings
// are then appended to the winner's config (see compose).
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

	base, err := r.resolveBase(ctx, namespace, decision, defaults)
	if err != nil {
		// Additives alone are served only when there is no base at all; a
		// failing base keeps failing so additives never mask it.
		if len(decision.Additives) == 0 || !errors.Is(err, port.ErrConfigNotFound) {
			return nil, err
		}
		base = nil
	}
	if len(decision.Additives) == 0 {
		return base, nil
	}
	return r.compose(ctx, namespace, base, decision.Additives)
}

// resolveBase returns the config of the Replace winner or the default.
func (r *ConfigResolver) resolveBase(
	ctx context.Context, namespace string, decision resolution.Decision, defaults []fleetv1alpha1.PipelineConfig,
) (*port.ResolvedConfig, error) {
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

// compose appends the usable additives to base (nil when there is no base).
// Whenever composing is not possible it serves the base as it was, because an
// additive must never change base serving.
func (r *ConfigResolver) compose(
	ctx context.Context, namespace string, base *port.ResolvedConfig, additives []*fleetv1alpha1.CollectorGroupBinding,
) (*port.ResolvedConfig, error) {
	log := logf.FromContext(ctx)
	contribs, err := r.selector.contributing(ctx, namespace, additives)
	if err != nil {
		log.Error(err, "Failed to load additive pipelines, serving base config")
		contribs = nil
	}
	parts := make([]composition.Additive, len(contribs))
	for i, c := range contribs {
		parts[i] = composition.Additive{BindingName: c.binding.Name, Content: c.content}
	}
	if len(parts) > 0 {
		var baseContent, baseHash string
		if base != nil {
			baseContent, baseHash = base.Content, base.ContentHash
		}
		res := composition.Compose(baseContent, baseHash, parts)
		first, perr := composition.CheckDocument(res.Hash, res.Content)
		if perr == nil {
			out := &port.ResolvedConfig{
				Content: res.Content, ContentHash: res.Hash, Path: port.PathAdditive, Additives: res.Included,
			}
			if base != nil {
				out.Path, out.BindingName = base.Path, base.BindingName
			}
			return out, nil
		}
		if first {
			log.Error(perr, "Composed config did not parse, serving base config", "hash", res.Hash)
		}
	}
	if base == nil {
		return nil, &port.ResolveError{Path: port.PathAdditive, Err: port.ErrConfigNotFound}
	}
	return base, nil
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
