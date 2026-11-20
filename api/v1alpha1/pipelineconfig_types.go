// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PipelineConfigSpec defines the desired state of PipelineConfig
type PipelineConfigSpec struct {
	// Content is the alloy pipeline configuration in raw format.
	// It must not be empty or whitespace only. Syntax is checked according to
	// the operator's --content-validation mode; unknown components and bad
	// references are never checked here, Alloy reports those itself.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self.trim() != ''",message="content must not be blank"
	Content string `json:"content"`

	// Description is an optional description of the pipeline configuration
	// While it is optional, we recommend that you provide a description
	// so that you can easily identify what the pipeline configuration does
	Description *string `json:"description,omitempty"`
}

// PipelineConfigStatus defines the observed state of PipelineConfig.
type PipelineConfigStatus struct {
	// ContentHash is the sha256 hex digest of spec.content, in every
	// validation mode.
	ContentHash string `json:"contentHash,omitempty"`
	// LastValidContent is the last spec.content that parsed. It is only set
	// when the operator runs with --content-validation=enforce, which serves
	// it instead of an invalid edit.
	// +optional
	LastValidContent string `json:"lastValidContent,omitempty"`
	// LastValidHash is the sha256 hex digest of LastValidContent.
	// +optional
	LastValidHash string `json:"lastValidHash,omitempty"`
	// ValidatedGeneration is the generation the operator last checked under
	// enforce. Zero means the object was never checked, which is how objects
	// that predate enforce are told apart from new ones.
	// +optional
	ValidatedGeneration int64 `json:"validatedGeneration,omitempty"`
	// ActiveBindings is the number of CollectorGroupBindings currently
	// referencing this PipelineConfig. Check this field before modifying
	// or deleting the config to understand the blast radius of the change.
	// It is derived from the bindings on every reconcile and self-heals.
	// +kubebuilder:validation:Minimum=0
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

// Condition types set on PipelineConfig.
const (
	// ConditionTypeContentValid is True when the content is non-blank and, if
	// content validation is on, parses. Reasons: ContentValid, EmptyContent,
	// SyntaxError.
	ConditionTypeContentValid = "ContentValid"

	// ConditionTypeSeededUnvalidated is True when enforce found content that
	// predates it and does not parse. That content keeps being served so
	// nothing that worked stops working, until a valid edit replaces it.
	ConditionTypeSeededUnvalidated = "SeededUnvalidated"

	// ConditionTypeContentTooLarge is True when the content is too big to
	// copy into status. Enforce then serves it with warn semantics.
	ConditionTypeContentTooLarge = "ContentTooLarge"
)

// MaxLastValidContentBytes is the largest content enforce copies into status.
// It leaves room for the spec copy under the 1.5 MiB etcd object limit.
const MaxLastValidContentBytes = 600 * 1024

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='ContentValid')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

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
