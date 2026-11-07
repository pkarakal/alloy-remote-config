// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package port

import "context"

// CollectorInfo describes a collector for registration purposes.
type CollectorInfo struct {
	ID              string
	Name            string
	LocalAttributes map[string]string
	// ConfigStatus and ConfigError carry the apply status from GetConfig. An
	// empty ConfigStatus (Register) means "leave the stored status untouched".
	ConfigStatus ConfigStatus
	ConfigError  string
}

// ConfigStatus is the apply status a collector reports for its remote config.
type ConfigStatus string

const (
	ConfigStatusUnknown  ConfigStatus = "Unknown"
	ConfigStatusApplying ConfigStatus = "Applying"
	ConfigStatusApplied  ConfigStatus = "Applied"
	ConfigStatusFailed   ConfigStatus = "Failed"
)

// CollectorRegistry tracks collector registrations.
type CollectorRegistry interface {
	// Register records the collector and always writes (explicit registration).
	Register(ctx context.Context, info CollectorInfo) error
	// Heartbeat refreshes liveness on each poll and may skip the write when the
	// tenant was refreshed recently, so polls do not amplify into API writes.
	Heartbeat(ctx context.Context, info CollectorInfo) error
	// Unregister removes the collector.
	Unregister(ctx context.Context, id string) error
}
