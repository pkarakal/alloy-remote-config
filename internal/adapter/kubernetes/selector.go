// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/controller"
	"github.com/pkarakal/alloy-remote-config/internal/resolution"
)

// bindingSelector gathers candidates from the informer cache and ranks them
// with the resolution package. The config resolver and the registry both use
// it, so they always agree on which binding wins.
type bindingSelector struct {
	reader client.Reader

	mu sync.Mutex
	// compiled caches parsed matchers per CollectorGroup UID and generation.
	compiled map[types.UID]compiledGroup
}

type compiledGroup struct {
	generation int64
	matchers   resolution.Matchers // nil when absent or invalid
}

func newBindingSelector(reader client.Reader) *bindingSelector {
	return &bindingSelector{reader: reader, compiled: map[types.UID]compiledGroup{}}
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
// binding applies.
func (s *bindingSelector) decide(
	ctx context.Context, namespace string, attrs map[string]string, hasDefault func(context.Context) (bool, error),
) (resolution.Decision, error) {
	bindings, groups, err := s.candidates(ctx, namespace, attrs)
	if err != nil {
		return resolution.Decision{}, err
	}
	in := resolution.Input{Attrs: attrs, Groups: groups, Bindings: bindings}
	if len(bindings) == 0 && hasDefault != nil {
		if in.HasDefault, err = hasDefault(ctx); err != nil {
			return resolution.Decision{}, err
		}
	}
	return resolution.Resolve(in), nil
}
