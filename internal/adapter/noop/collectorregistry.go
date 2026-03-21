// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package noop

import (
	"context"

	"github.com/pkarakal/alloy-remote-config/internal/port"
)

// CollectorRegistry is a no-op implementation of port.CollectorRegistry.
// It acknowledges registrations without persisting them.
type CollectorRegistry struct{}

var _ port.CollectorRegistry = (*CollectorRegistry)(nil)

func (r *CollectorRegistry) Register(_ context.Context, _ port.CollectorInfo) error {
	return nil
}

func (r *CollectorRegistry) Unregister(_ context.Context, _ string) error {
	return nil
}
