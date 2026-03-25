// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package v1alpha1

const (
	// LabelDefaultPipelineConfig marks a PipelineConfig as the cluster-wide default.
	// ResolveDefault queries by this label instead of by name, so the resource
	// can be freely renamed without breaking config resolution.
	LabelDefaultPipelineConfig = "fleet.pkarakal.com/default-pipeline-config"

	// LabelDefaultCollectorGroup marks a CollectorGroup as the cluster-wide default.
	LabelDefaultCollectorGroup = "fleet.pkarakal.com/default-collector-group"
)
