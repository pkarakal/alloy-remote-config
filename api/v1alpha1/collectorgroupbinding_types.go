// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CollectorGroupBindingSpec defines the desired state of CollectorGroupBinding
// +kubebuilder:validation:XValidation:rule="(has(self.collectorGroupRef) && self.collectorGroupRef != \"\") != (has(self.tenantRef) && self.tenantRef != \"\")",message="exactly one of collectorGroupRef or tenantRef must be set"
type CollectorGroupBindingSpec struct {
	// CollectorGroupRef is the name of the CollectorGroup this binding
	// targets. All collectors that self-identify with this group via
	// their remotecfg attributes will receive the referenced config.
	// Mutually exclusive with TenantRef.
	// +optional
	CollectorGroupRef string `json:"collectorGroupRef,omitempty"`

	// TenantRef is the tenant ID of a specific collector to target.
	// Takes precedence over CollectorGroupRef during config resolution
	// if both a tenant-specific and group-wide binding exist.
	// Mutually exclusive with CollectorGroupRef.
	// +optional
	TenantRef string `json:"tenantRef,omitempty"`

	// PipelineConfigRef is the name of the PipelineConfig to serve
	// to collectors matched by this binding.
	// +required
	PipelineConfigRef string `json:"pipelineConfigRef"`
}

// CollectorGroupBindingStatus defines the observed state of CollectorGroupBinding.
type CollectorGroupBindingStatus struct {
	// Phase summarises the current state of this binding.
	Phase CollectorGroupBindingPhase `json:"phase,omitempty"`

	// ConfigHash is the SHA256 hash of the PipelineConfig content
	// currently being served to matched collectors. Matches the hash
	// field in GetConfigResponse — compare against remotecfg_* metrics
	// to verify collectors have picked up the latest config.
	ConfigHash string `json:"configHash,omitempty"`

	// LastSyncedAt is the time at which the operator last successfully
	// projected this binding into the in-memory index.
	LastSyncedAt metav1.Time `json:"lastSyncedAt,omitempty"`

	// Conditions contains the current observed conditions of this binding.
	// RefsValid: both collectorGroupRef/tenantRef and pipelineConfigRef
	//   resolve to existing resources.
	// Indexed: this binding is actively projected into the serving index
	//   and being served to matched collectors.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last generation of this binding that
	// the operator has successfully reconciled. If this value is less
	// than metadata.generation, the current status reflects a previous
	// version of the spec and the reconciler has not yet processed the
	// latest changes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

type CollectorGroupBindingPhase string

const (
	// BindingPhasePending indicates the binding has not yet been
	// reconciled by the operator.
	BindingPhasePending CollectorGroupBindingPhase = "Pending"

	// BindingPhaseActive indicates the binding is healthy, all
	// referenced resources exist, and it is actively being served.
	BindingPhaseActive CollectorGroupBindingPhase = "Active"

	// BindingPhaseDegraded indicates the binding cannot be served,
	// either because a referenced resource does not exist or the
	// operator failed to index it. See Conditions for details.
	BindingPhaseDegraded CollectorGroupBindingPhase = "Degraded"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// CollectorGroupBinding is the Schema for the collectorgroupbindings API
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Config",type="string",JSONPath=".spec.pipelineConfigRef"
// +kubebuilder:printcolumn:name="Hash",type="string",JSONPath=".status.configHash"
// +kubebuilder:printcolumn:name="Synced",type="date",JSONPath=".status.lastSyncedAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.collectorGroupRef) && self.spec.collectorGroupRef != \"\") != (has(self.spec.tenantRef) && self.spec.tenantRef != \"\")",message="exactly one of collectorGroupRef or tenantRef must be set"
type CollectorGroupBinding struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CollectorGroupBinding
	// +required
	Spec CollectorGroupBindingSpec `json:"spec"`

	// status defines the observed state of CollectorGroupBinding
	// +optional
	Status CollectorGroupBindingStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CollectorGroupBindingList contains a list of CollectorGroupBinding
type CollectorGroupBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CollectorGroupBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CollectorGroupBinding{}, &CollectorGroupBindingList{})
}
