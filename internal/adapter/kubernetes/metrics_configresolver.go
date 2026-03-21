// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"errors"
	"time"

	"github.com/pkarakal/alloy-remote-config/internal/metrics"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

// MetricsConfigResolver wraps a port.ConfigResolver and records resolution
// metrics for each step in the fallback chain.
type MetricsConfigResolver struct {
	inner   port.ConfigResolver
	metrics *metrics.ResolutionMetrics
}

var _ port.ConfigResolver = (*MetricsConfigResolver)(nil)

// NewMetricsConfigResolver creates a MetricsConfigResolver that decorates inner
// with the given resolution metrics.
func NewMetricsConfigResolver(inner port.ConfigResolver, m *metrics.ResolutionMetrics) *MetricsConfigResolver {
	return &MetricsConfigResolver{inner: inner, metrics: m}
}

func (m *MetricsConfigResolver) ResolveByTenant(ctx context.Context, namespace, tenant string) (*port.ResolvedConfig, error) {
	return m.observe("tenant", func() (*port.ResolvedConfig, error) {
		return m.inner.ResolveByTenant(ctx, namespace, tenant)
	})
}

func (m *MetricsConfigResolver) ResolveByCollectorGroup(ctx context.Context, namespace, collectorGroup string) (*port.ResolvedConfig, error) {
	return m.observe("collectorgroup", func() (*port.ResolvedConfig, error) {
		return m.inner.ResolveByCollectorGroup(ctx, namespace, collectorGroup)
	})
}

func (m *MetricsConfigResolver) ResolveDefault(ctx context.Context, namespace string) (*port.ResolvedConfig, error) {
	return m.observe("default", func() (*port.ResolvedConfig, error) {
		return m.inner.ResolveDefault(ctx, namespace)
	})
}

func (m *MetricsConfigResolver) observe(resolutionPath string, fn func() (*port.ResolvedConfig, error)) (*port.ResolvedConfig, error) {
	start := time.Now()
	result, err := fn()
	elapsed := time.Since(start).Seconds()

	m.metrics.Total.WithLabelValues(resolutionPath, classifyOutcome(err)).Inc()
	m.metrics.Duration.WithLabelValues(resolutionPath).Observe(elapsed)

	return result, err
}

// classifyOutcome maps a resolution error to a label value for the outcome dimension.
func classifyOutcome(err error) string {
	if err == nil {
		return "found"
	}
	switch {
	case errors.Is(err, port.ErrConfigNotFound):
		return "not_found"
	case errors.Is(err, port.ErrConfigNotReady):
		return "not_ready"
	default:
		return "error"
	}
}
