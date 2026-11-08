// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package validate

import (
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

// Served returns the content and hash to serve for pc, and false when the
// config is not ready. Outside enforce mode that is the spec content with
// status.contentHash.
//
// In enforce the last valid content wins. Without one, the object is not
// ready only if its ContentValid condition is False; otherwise the spec is
// served (seeded or oversized objects, and clusters whose CRD predates the
// lastValid fields, where enforce degrades to warn). This is decided by
// conditions because they survive schema pruning.
func Served(pc *fleetv1alpha1.PipelineConfig, mode Mode) (content, hash string, ok bool) {
	// Blank content can predate the CRD rule; never serve it.
	if pc.Status.ContentHash == "" || strings.TrimSpace(pc.Spec.Content) == "" {
		return "", "", false
	}
	if mode != ModeEnforce {
		return pc.Spec.Content, pc.Status.ContentHash, true
	}
	if pc.Status.LastValidContent != "" {
		return pc.Status.LastValidContent, pc.Status.LastValidHash, true
	}
	if meta.IsStatusConditionTrue(pc.Status.Conditions, fleetv1alpha1.ConditionTypeSeededUnvalidated) ||
		meta.IsStatusConditionTrue(pc.Status.Conditions, fleetv1alpha1.ConditionTypeContentTooLarge) {
		return pc.Spec.Content, pc.Status.ContentHash, true
	}
	if meta.IsStatusConditionPresentAndEqual(pc.Status.Conditions, fleetv1alpha1.ConditionTypeContentValid, metav1.ConditionFalse) {
		return "", "", false
	}
	return pc.Spec.Content, pc.Status.ContentHash, true
}
