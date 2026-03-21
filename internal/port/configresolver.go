// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package port

import (
	"context"
	"errors"
)

var (
	// ErrConfigNotFound is returned when no matching configuration exists for the
	// given lookup key. The service layer uses this sentinel to fall through to
	// the next resolution step.
	ErrConfigNotFound = errors.New("no matching configuration found")

	// ErrConfigNotReady is returned when a PipelineConfig exists but has not yet
	// been reconciled (ContentHash is empty). The service layer maps this to
	// CodeUnavailable so the caller knows to retry later.
	ErrConfigNotReady = errors.New("configuration exists but is not yet reconciled")
)

// ResolvedConfig holds the content and hash of a successfully resolved
// pipeline configuration.
type ResolvedConfig struct {
	Content     string
	ContentHash string
}

// ConfigResolver resolves pipeline configurations in a storage-agnostic way.
// Each method corresponds to one step in the fallback chain:
// tenant -> collector group -> default.
type ConfigResolver interface {
	ResolveByTenant(ctx context.Context, namespace, tenant string) (*ResolvedConfig, error)
	ResolveByCollectorGroup(ctx context.Context, namespace, collectorGroup string) (*ResolvedConfig, error)
	ResolveDefault(ctx context.Context, namespace string) (*ResolvedConfig, error)
}
