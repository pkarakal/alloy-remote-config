// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CollectorGroupSpec defines the desired state of CollectorGroup
type CollectorGroupSpec struct {
	// Human-readable description of what this group represents
	// e.g. "Production tenants running the core streaming pipeline"
	Description string `json:"description,omitempty"`

	// Matchers define membership by collector attributes, in addition to the
	// legacy rule (attribute collector_group equals this group's name).
	// Each item is `key OP "value"` with OP one of =, !=, =~, !~. Regexes are
	// fully anchored, a missing attribute equals "", and all matchers must match.
	// Example: org_id="org-1234", cluster_id=~"c-1|c-2".
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=200
	// +kubebuilder:validation:items:XValidation:rule="self.matches('^[a-zA-Z_][a-zA-Z0-9_.]*\\\\s*(=|!=|=~|!~)\\\\s*\".*\"$')",message="matcher must look like key=\"value\" (operators: =, !=, =~, !~)"
	Matchers []string `json:"matchers,omitempty"`
}

// CollectorGroupStatus defines the observed state of CollectorGroup.
type CollectorGroupStatus struct {
	// ActiveBindings is the number of CollectorGroupBindings currently
	// referencing this CollectorGroup. Check this field before deleting
	// the group to understand which bindings would be left dangling.
	// It is derived from the bindings on every reconcile and self-heals.
	// +kubebuilder:validation:Minimum=0
	ActiveBindings int `json:"activeBindings"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions contains the observed conditions of this group.
	// MatchersValid: spec.matchers compile. It is only set when matchers are
	//   present. When False the group contributes only legacy membership.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ConditionTypeMatchersValid reports whether spec.matchers compiled.
const ConditionTypeMatchersValid = "MatchersValid"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// CollectorGroup is the Schema for the collectorgroups API
type CollectorGroup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CollectorGroup
	// +required
	Spec CollectorGroupSpec `json:"spec"`

	// status defines the observed state of CollectorGroup
	// +optional
	Status CollectorGroupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CollectorGroupList contains a list of CollectorGroup
type CollectorGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CollectorGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CollectorGroup{}, &CollectorGroupList{})
}
