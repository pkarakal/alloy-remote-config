// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package port

import "context"

// CollectorInfo describes a collector for registration purposes.
type CollectorInfo struct {
	ID              string
	Name            string
	LocalAttributes map[string]string
}

// CollectorRegistry tracks collector registrations. The current implementation
// is a no-op; a CRD-backed or database-backed implementation can be added later.
type CollectorRegistry interface {
	Register(ctx context.Context, info CollectorInfo) error
	Unregister(ctx context.Context, id string) error
}
