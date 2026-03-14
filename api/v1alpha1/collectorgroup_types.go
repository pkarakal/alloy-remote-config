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
}

// CollectorGroupStatus defines the observed state of CollectorGroup.
type CollectorGroupStatus struct {
	// ActiveBindings is the number of CollectorGroupBindings currently
	// referencing this CollectorGroup. Check this field before deleting
	// the group to understand which bindings would be left dangling.
	ActiveBindings int `json:"activeBindings"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

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
