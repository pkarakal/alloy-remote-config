// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package port

import (
	"context"
	"errors"
)

var (
	// ErrConfigNotFound is returned when no matching configuration exists for the
	// given attributes and no default config exists.
	ErrConfigNotFound = errors.New("no matching configuration found")

	// ErrConfigNotReady is returned when a PipelineConfig exists but has not yet
	// been reconciled (ContentHash is empty). The service layer maps this to
	// CodeUnavailable so the caller knows to retry later.
	ErrConfigNotReady = errors.New("configuration exists but is not yet reconciled")
)

// Resolution paths reported in ResolvedConfig.Path and ResolveError.Path.
const (
	PathTenant         = "tenant"
	PathCollectorGroup = "collectorgroup"
	PathDefault        = "default"
	// PathAdditive is used when only additive bindings supply the config.
	PathAdditive = "additive"
)

// ResolvedConfig holds the content and hash of a successfully resolved
// pipeline configuration, and which candidate supplied it.
type ResolvedConfig struct {
	Content     string
	ContentHash string
	// Path is the kind of the winning candidate (PathTenant, PathCollectorGroup
	// or PathDefault).
	Path string
	// BindingName is the winning CollectorGroupBinding; empty for the default.
	BindingName string
	// Additives names the Additive bindings composed into Content, in name order.
	Additives []string
}

// ResolveError carries the winning candidate when resolution fails after one
// was chosen (for example a dangling PipelineConfigRef), so callers such as
// metrics still know which path failed. It unwraps to the cause.
type ResolveError struct {
	Path        string
	BindingName string
	Err         error
}

func (e *ResolveError) Error() string { return e.Err.Error() }
func (e *ResolveError) Unwrap() error { return e.Err }

// ConfigResolver resolves pipeline configurations in a storage-agnostic way.
type ConfigResolver interface {
	// Resolve picks the highest-priority binding that applies to a collector
	// with the given attributes, or the default config when none applies. The
	// winner decides: an unusable winner is an error, never a fallthrough.
	Resolve(ctx context.Context, namespace string, attrs map[string]string) (*ResolvedConfig, error)
}
