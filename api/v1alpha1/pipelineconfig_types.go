// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PipelineConfigSpec defines the desired state of PipelineConfig
type PipelineConfigSpec struct {
	// Content is the alloy pipeline configuration in raw format.
	// No validation is performed on the content and it is up to
	// the tenant Alloys to report any syntax errors
	Content string `json:"content"`

	// Description is an optional description of the pipeline configuration
	// While it is optional, we recommend that you provide a description
	// so that you can easily identify what the pipeline configuration does
	Description *string `json:"description,omitempty"`
}

// PipelineConfigStatus defines the observed state of PipelineConfig.
type PipelineConfigStatus struct {
	// ContentHash is the hash of the content field.
	ContentHash string `json:"contentHash,omitempty"`
	// ActiveBindings is the number of CollectorGroupBindings currently
	// referencing this PipelineConfig. Check this field before modifying
	// or deleting the config to understand the blast radius of the change.
	ActiveBindings int `json:"activeBindings"`
	// ObservedGeneration is the last generation of this PipelineConfig
	// that the operator has successfully reconciled. If this value is less
	// than metadata.generation, the current status reflects a previous
	// version of the spec and the reconciler has not yet processed the
	// latest changes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// conditions represent the current state of the PipelineConfig resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// PipelineConfig is the Schema for the pipelineconfigs API
type PipelineConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PipelineConfig
	// +required
	Spec PipelineConfigSpec `json:"spec"`

	// status defines the observed state of PipelineConfig
	// +optional
	Status PipelineConfigStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PipelineConfigList contains a list of PipelineConfig
type PipelineConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PipelineConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PipelineConfig{}, &PipelineConfigList{})
}
