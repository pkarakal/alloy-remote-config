// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/composition"
	"github.com/pkarakal/alloy-remote-config/internal/controller"
	"github.com/pkarakal/alloy-remote-config/internal/resolution"
)

// bindingSelector gathers candidates from the informer cache and ranks them
// with the resolution package. The config resolver and the registry both use
// it, so they always agree on which binding wins.
type bindingSelector struct {
	reader client.Reader
	// now is the expiry clock.
	now func() time.Time

	mu sync.Mutex
	// compiled caches parsed matchers per CollectorGroup UID and generation.
	compiled map[types.UID]compiledGroup
}

type compiledGroup struct {
	generation int64
	matchers   resolution.Matchers // nil when absent or invalid
}

func newBindingSelector(reader client.Reader) *bindingSelector {
	return &bindingSelector{reader: reader, now: time.Now, compiled: map[types.UID]compiledGroup{}}
}

// groups lists the namespace's CollectorGroups with their compiled matchers.
// Invalid matchers yield a group without matchers (legacy membership only).
func (s *bindingSelector) groups(ctx context.Context, namespace string) ([]resolution.Group, error) {
	list := &fleetv1alpha1.CollectorGroupList{}
	if err := s.reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing CollectorGroups: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]resolution.Group, 0, len(list.Items))
	live := make(map[types.UID]struct{}, len(list.Items))
	for i := range list.Items {
		g := &list.Items[i]
		live[g.UID] = struct{}{}
		c, ok := s.compiled[g.UID]
		if !ok || c.generation != g.Generation {
			c = compiledGroup{generation: g.Generation}
			if len(g.Spec.Matchers) > 0 {
				if m, err := resolution.ParseMatchers(g.Spec.Matchers); err == nil {
					c.matchers = m
				}
			}
			s.compiled[g.UID] = c
		}
		out = append(out, resolution.Group{Name: g.Name, Matchers: c.matchers})
	}
	// Drop entries of deleted groups.
	if len(s.compiled) > len(live) {
		for uid := range s.compiled {
			if _, ok := live[uid]; !ok {
				delete(s.compiled, uid)
			}
		}
	}
	return out, nil
}

// candidates returns the tenant bindings and the bindings of every group the
// collector belongs to, using the field indexes.
func (s *bindingSelector) candidates(
	ctx context.Context, namespace string, attrs map[string]string,
) ([]fleetv1alpha1.CollectorGroupBinding, []resolution.Group, error) {
	groups, err := s.groups(ctx, namespace)
	if err != nil {
		return nil, nil, err
	}
	var bindings []fleetv1alpha1.CollectorGroupBinding
	if tenant := attrs[resolution.AttrTenant]; tenant != "" {
		list := &fleetv1alpha1.CollectorGroupBindingList{}
		if err := s.reader.List(ctx, list,
			client.InNamespace(namespace),
			client.MatchingFields{controller.IndexTenantRef: tenant},
		); err != nil {
			return nil, nil, fmt.Errorf("listing tenant bindings: %w", err)
		}
		bindings = append(bindings, list.Items...)
	}
	for _, name := range resolution.Members(attrs, groups) {
		list := &fleetv1alpha1.CollectorGroupBindingList{}
		if err := s.reader.List(ctx, list,
			client.InNamespace(namespace),
			client.MatchingFields{controller.IndexCollectorGroupRef: name},
		); err != nil {
			return nil, nil, fmt.Errorf("listing group bindings: %w", err)
		}
		bindings = append(bindings, list.Items...)
	}
	return bindings, groups, nil
}

// decide runs the resolution. hasDefault is consulted lazily, only when no
// Replace binding applies (none targets the collector, or all have expired).
func (s *bindingSelector) decide(
	ctx context.Context, namespace string, attrs map[string]string, hasDefault func(context.Context) (bool, error),
) (resolution.Decision, error) {
	bindings, groups, err := s.candidates(ctx, namespace, attrs)
	if err != nil {
		return resolution.Decision{}, err
	}
	in := resolution.Input{Attrs: attrs, Groups: groups, Bindings: bindings, Now: s.now()}
	d := resolution.Resolve(in)
	if d.Kind == resolution.KindNone && hasDefault != nil {
		if in.HasDefault, err = hasDefault(ctx); err != nil {
			return resolution.Decision{}, err
		}
		if in.HasDefault {
			d = resolution.Resolve(in)
		}
	}
	return d, nil
}

// contribution is an Additive binding that will be composed.
type contribution struct {
	binding *fleetv1alpha1.CollectorGroupBinding
	content string
}

// contributing keeps the additives that can be composed: their PipelineConfig
// exists, is reconciled, and parses inside a declare block. Others are skipped
// silently since they must never affect the base. The result keeps name order.
func (s *bindingSelector) contributing(
	ctx context.Context, namespace string, additives []*fleetv1alpha1.CollectorGroupBinding,
) ([]contribution, error) {
	out := make([]contribution, 0, len(additives))
	for _, b := range additives {
		pc := &fleetv1alpha1.PipelineConfig{}
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: b.Spec.PipelineConfigRef}, pc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("fetching PipelineConfig %q for additive binding %q: %w",
				b.Spec.PipelineConfigRef, b.Name, err)
		}
		if pc.Status.ContentHash == "" || strings.TrimSpace(pc.Spec.Content) == "" {
			continue
		}
		if composition.CheckSnippet(pc.Spec.Content) != nil {
			continue
		}
		out = append(out, contribution{binding: b, content: pc.Spec.Content})
	}
	return out, nil
}
