// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

// Package selection picks one object out of several competing candidates in a
// way that is stable across cache list order and process restarts.
package selection

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Older reports whether a wins over b: the earlier creationTimestamp wins and
// ties (the API server has 1s resolution) fall back to the lower name.
func Older(a, b metav1.Object) bool {
	at, bt := a.GetCreationTimestamp(), b.GetCreationTimestamp()
	if !at.Equal(&bt) {
		return at.Before(&bt)
	}
	return a.GetName() < b.GetName()
}

// Pick returns a pointer to the winning element of items, or nil when items is
// empty. The resolver, the registry and the Overlap condition all use it so
// they always agree on the winner.
func Pick[T any, PT interface {
	*T
	metav1.Object
}](items []T) *T {
	var best *T
	for i := range items {
		if best == nil || Older(PT(&items[i]), PT(best)) {
			best = &items[i]
		}
	}
	return best
}
