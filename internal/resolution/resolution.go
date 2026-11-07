// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package resolution

import (
	"slices"
	"strings"
	"time"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/selection"
)

const (
	// AttrTenant and AttrCollectorGroup are the remotecfg attributes that
	// select tenant bindings and legacy group membership.
	AttrTenant         = "tenant"
	AttrCollectorGroup = "collector_group"

	// DefaultTenantPriority, DefaultGroupPriority and DefaultConfigPriority are
	// the effective priorities used when a binding sets none.
	DefaultTenantPriority int32 = 200
	DefaultGroupPriority  int32 = 100
	DefaultConfigPriority int32 = 0
)

// Kind says what kind of candidate won.
type Kind string

const (
	KindNone   Kind = ""
	KindTenant Kind = "tenant"
	KindGroup  Kind = "collectorgroup"
	// KindDefault is the default-labeled PipelineConfig.
	KindDefault Kind = "default"
)

// Group is a CollectorGroup reduced to what membership needs. Matchers is nil
// when the group has none or they are invalid.
type Group struct {
	Name     string
	Matchers Matchers
}

// Input is everything Resolve needs. Bindings may be a superset of the real
// candidates; Resolve filters them by tenant and membership.
type Input struct {
	Attrs      map[string]string
	Groups     []Group
	Bindings   []fleetv1alpha1.CollectorGroupBinding
	HasDefault bool
	// Now is the clock for expiry. The zero value disables expiry checks.
	Now time.Time
}

// Decision is the outcome. Binding is set for tenant and group winners.
type Decision struct {
	Kind     Kind
	Binding  *fleetv1alpha1.CollectorGroupBinding
	Priority int32
	// Additives are the Additive bindings that target the collector and have
	// not expired, sorted by name. They never compete for Binding.
	Additives []*fleetv1alpha1.CollectorGroupBinding
}

// IsAdditive reports whether the binding appends to the base instead of
// competing for it.
func IsAdditive(b *fleetv1alpha1.CollectorGroupBinding) bool {
	return b.Spec.Mode == fleetv1alpha1.BindingModeAdditive
}

// IsExpired reports whether spec.expiresAt is at or before now. A zero now
// never expires anything.
func IsExpired(b *fleetv1alpha1.CollectorGroupBinding, now time.Time) bool {
	return !now.IsZero() && b.Spec.ExpiresAt != nil && !b.Spec.ExpiresAt.After(now)
}

// EffectivePriority returns spec.priority or the default for the binding kind.
func EffectivePriority(b *fleetv1alpha1.CollectorGroupBinding) int32 {
	if b.Spec.Priority != nil {
		return *b.Spec.Priority
	}
	if b.Spec.TenantRef != "" {
		return DefaultTenantPriority
	}
	return DefaultGroupPriority
}

// Members returns the names of the groups the collector belongs to: the legacy
// collector_group attribute (even when no CollectorGroup object exists) plus
// every group whose valid matchers all match. The result is sorted and unique.
func Members(attrs map[string]string, groups []Group) []string {
	var names []string
	if g := attrs[AttrCollectorGroup]; g != "" {
		names = append(names, g)
	}
	for _, g := range groups {
		if g.Matchers.MatchAll(attrs) {
			names = append(names, g.Name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// Better reports whether a outranks b: higher effective priority first, then
// the shared oldest-then-name tie-break.
func Better(a, b *fleetv1alpha1.CollectorGroupBinding) bool {
	if pa, pb := EffectivePriority(a), EffectivePriority(b); pa != pb {
		return pa > pb
	}
	return selection.Older(a, b)
}

// targets reports whether the binding addresses the collector.
func targets(b *fleetv1alpha1.CollectorGroupBinding, tenant string, members []string) bool {
	switch {
	case b.Spec.TenantRef != "":
		return tenant != "" && b.Spec.TenantRef == tenant
	case b.Spec.CollectorGroupRef != "":
		return slices.Contains(members, b.Spec.CollectorGroupRef)
	}
	return false
}

// Resolve picks the top Replace binding. As before, the top candidate decides:
// callers must not fall through when its PipelineConfig is unusable. Expired
// bindings are not candidates, so an expired winner falls back to the next one.
// With no Replace candidate it falls back to the default config. Additive
// bindings never compete; they are returned in Decision.Additives.
func Resolve(in Input) Decision {
	tenant := in.Attrs[AttrTenant]
	members := Members(in.Attrs, in.Groups)

	var best *fleetv1alpha1.CollectorGroupBinding
	var additives []*fleetv1alpha1.CollectorGroupBinding
	for i := range in.Bindings {
		b := &in.Bindings[i]
		if IsExpired(b, in.Now) || !targets(b, tenant, members) {
			continue
		}
		if IsAdditive(b) {
			additives = append(additives, b)
			continue
		}
		if best == nil || Better(b, best) {
			best = b
		}
	}
	slices.SortFunc(additives, func(a, b *fleetv1alpha1.CollectorGroupBinding) int {
		return strings.Compare(a.Name, b.Name)
	})
	d := Decision{Additives: additives}
	switch {
	case best != nil:
		d.Kind = KindGroup
		if best.Spec.TenantRef != "" {
			d.Kind = KindTenant
		}
		d.Binding = best
		d.Priority = EffectivePriority(best)
	case in.HasDefault:
		d.Kind = KindDefault
		d.Priority = DefaultConfigPriority
	}
	return d
}
