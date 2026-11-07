// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"strings"

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

	// Priority ranks this binding against others that match the same
	// collector; the highest wins. When unset it defaults to 200 for tenant
	// bindings and 100 for group bindings.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	Priority *int32 `json:"priority,omitempty"`
}

// RegisteredTenant records a tenant that has called RegisterCollector and has
// been matched to this binding. Entries are evicted by the reconciler when
// LastSeenAt is older than the configured collector TTL.
type RegisteredTenant struct {
	// ID is the tenant ID from LocalAttributes["tenant"].
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`

	// CollectorID is the collector instance ID from RegisterCollectorRequest.Id.
	// Stored so that UnregisterCollector (which only carries the collector ID)
	// can find and remove this entry.
	// +kubebuilder:validation:Required
	CollectorID string `json:"collectorId"`

	// LastSeenAt is the time the collector last called RegisterCollector for this tenant.
	LastSeenAt metav1.Time `json:"lastSeenAt"`

	// ConfigStatus is the apply status the collector last reported through
	// GetConfig (Alloy v1.12 or newer). The entry reflects whichever collector
	// instance wrote last for this tenant.
	// +optional
	ConfigStatus ConfigStatus `json:"configStatus,omitempty"`

	// LastError is the error message from the last Failed report, truncated to
	// 1024 bytes. Empty when the collector is not failing.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	LastError string `json:"lastError,omitempty"`

	// StatusChangedAt is the time configStatus or lastError last changed.
	// +optional
	StatusChangedAt *metav1.Time `json:"statusChangedAt,omitempty"`
}

// ConfigStatus is the apply status a collector reports for its remote config.
// +kubebuilder:validation:Enum=Unknown;Applying;Applied;Failed
type ConfigStatus string

const (
	// ConfigStatusUnknown means the collector reported no status (UNSET or an
	// older Alloy that does not report one).
	ConfigStatusUnknown ConfigStatus = "Unknown"
	// ConfigStatusApplying means the collector is loading the config.
	ConfigStatusApplying ConfigStatus = "Applying"
	// ConfigStatusApplied means the collector loaded the config successfully.
	ConfigStatusApplied ConfigStatus = "Applied"
	// ConfigStatusFailed means the collector failed to fetch or load the config.
	ConfigStatusFailed ConfigStatus = "Failed"
)

// MaxLastErrorBytes bounds RegisteredTenant.LastError and
// CollectorGroupBindingStatus.LastApplyError.
const MaxLastErrorBytes = 1024

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

	// BoundPipelineConfigRef tracks the PipelineConfigRef used in the last
	// successful reconcile. Informational only; activeBindings is derived
	// from the spec refs.
	BoundPipelineConfigRef string `json:"boundPipelineConfigRef,omitempty"`

	// BoundCollectorGroupRef tracks the CollectorGroupRef used in the last
	// successful reconcile. Informational only; activeBindings is derived
	// from the spec refs.
	BoundCollectorGroupRef string `json:"boundCollectorGroupRef,omitempty"`

	// EffectivePriority is spec.priority, or the default for the binding kind
	// (200 for tenant bindings, 100 for group bindings) when unset.
	// +optional
	EffectivePriority int32 `json:"effectivePriority,omitempty"`

	// RegisteredTenants is the list of tenants currently registered against
	// this binding. Entries are upserted on RegisterCollector and removed on
	// UnregisterCollector or when the entry has not been refreshed within the
	// collector TTL window.
	// +optional
	// +listType=map
	// +listMapKey=id
	RegisteredTenants []RegisteredTenant `json:"registeredTenants,omitempty"`

	// AppliedCount is the number of registered tenants whose last reported
	// status is Applied. It counts tenants, not collector pods.
	// +optional
	AppliedCount int32 `json:"appliedCount,omitempty"`

	// FailedCount is the number of registered tenants whose last reported
	// status is Failed. It counts tenants, not collector pods.
	// +optional
	FailedCount int32 `json:"failedCount,omitempty"`

	// LastApplyError is the error of the most recently changed Failed tenant,
	// truncated to 1024 bytes.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	LastApplyError string `json:"lastApplyError,omitempty"`
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

const (
	// ConditionTypeRefsValid indicates both PipelineConfig and
	// CollectorGroup/TenantRef resolve to existing resources.
	ConditionTypeRefsValid = "RefsValid"

	// ConditionTypeOverlap is True on a binding that loses to an older binding
	// with the same tenantRef or collectorGroupRef and the same effective
	// priority in the same namespace. Different priorities are an explicit
	// ranking, not an overlap.
	ConditionTypeOverlap = "Overlap"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// CollectorGroupBinding is the Schema for the collectorgroupbindings API
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Config",type="string",JSONPath=".spec.pipelineConfigRef"
// +kubebuilder:printcolumn:name="Priority",type="integer",JSONPath=".status.effectivePriority"
// +kubebuilder:printcolumn:name="Hash",type="string",JSONPath=".status.configHash"
// +kubebuilder:printcolumn:name="Applied",type="integer",JSONPath=".status.appliedCount"
// +kubebuilder:printcolumn:name="Failed",type="integer",JSONPath=".status.failedCount"
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

// TruncateError caps s at MaxLastErrorBytes without splitting a UTF-8 rune.
func TruncateError(s string) string {
	if len(s) <= MaxLastErrorBytes {
		return s
	}
	return strings.ToValidUTF8(s[:MaxLastErrorBytes], "")
}
