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
// metrics labeled by the kind of the winning candidate.
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

// Resolve records one observation per request, labeled with the kind of the
// winning candidate so existing dashboards keep working.
func (m *MetricsConfigResolver) Resolve(ctx context.Context, namespace string, attrs map[string]string) (*port.ResolvedConfig, error) {
	start := time.Now()
	result, err := m.inner.Resolve(ctx, namespace, attrs)
	elapsed := time.Since(start).Seconds()

	path := port.PathDefault
	if result != nil {
		path = result.Path
	} else if re := (*port.ResolveError)(nil); errors.As(err, &re) {
		path = re.Path
	}
	outcome := classifyOutcome(err)
	m.metrics.Total.WithLabelValues(path, outcome).Inc()
	m.metrics.Duration.WithLabelValues(path, outcome).Observe(elapsed)
	if result != nil {
		m.metrics.AdditivePipelines.Observe(float64(len(result.Additives)))
	}

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
