// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package metrics

import (
	"runtime"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"
)

// metricsNamespace is the common prefix for all custom metrics in this operator.
const metricsNamespace = "alloy_remote_config"

// rpcDurationBuckets covers the expected latency range for Connect-RPC calls
// that traverse the network and execute handler logic (sub-millisecond to seconds).
var rpcDurationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5}

// resolutionDurationBuckets covers the expected latency range for informer-cache
// lookups, which are pure in-memory operations and should complete in well under 1ms.
var resolutionDurationBuckets = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05}

// RPCMetrics holds Prometheus metrics for the Connect-RPC service layer.
type RPCMetrics struct {
	// Requests counts all RPC calls, partitioned by procedure and Connect status code.
	Requests *prometheus.CounterVec
	// Duration measures latency per procedure.
	Duration *prometheus.HistogramVec
	// Served counts GetConfig calls that returned a fresh configuration.
	Served prometheus.Counter
	// NotModified counts GetConfig calls where the collector's hash matched the current config.
	NotModified prometheus.Counter
}

// NewRPCMetrics creates an initialised RPCMetrics instance ready to be registered.
func NewRPCMetrics() *RPCMetrics {
	return &RPCMetrics{
		Requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Name:      "rpc_requests_total",
				Help:      "Total number of Connect-RPC requests, partitioned by procedure and response status code.",
			},
			[]string{"procedure", "code"},
		),
		Duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: metricsNamespace,
				Name:      "rpc_request_duration_seconds",
				Help:      "Histogram of Connect-RPC request duration in seconds, partitioned by procedure.",
				Buckets:   rpcDurationBuckets,
			},
			[]string{"procedure"},
		),
		Served: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "config_served_total",
			Help:      "Total number of GetConfig calls that returned a fresh configuration to the collector.",
		}),
		NotModified: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "config_not_modified_total",
			Help:      "Total number of GetConfig calls where the collector's hash matched the current configuration (304-equivalent).",
		}),
	}
}

// ResolutionMetrics holds Prometheus metrics for the config resolution chain.
type ResolutionMetrics struct {
	// Total counts resolution attempts partitioned by path (tenant/collectorgroup/default) and outcome.
	Total *prometheus.CounterVec
	// Duration measures resolution latency per path.
	Duration *prometheus.HistogramVec
}

// NewResolutionMetrics creates an initialised ResolutionMetrics instance ready to be registered.
func NewResolutionMetrics() *ResolutionMetrics {
	return &ResolutionMetrics{
		Total: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Name:      "config_resolution_total",
				Help:      "Total number of config resolution attempts, partitioned by resolution path and outcome.",
			},
			[]string{"path", "outcome"},
		),
		Duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: metricsNamespace,
				Name:      "config_resolution_duration_seconds",
				Help:      "Histogram of config resolution duration in seconds, partitioned by resolution path.",
				Buckets:   resolutionDurationBuckets,
			},
			[]string{"path"},
		),
	}
}

// BuildInfoMetric holds the operator build information gauge.
// It emits a single time series with value 1 and labels describing the build,
// following the standard Prometheus convention for info metrics.
type BuildInfoMetric struct {
	Info *prometheus.GaugeVec
}

// NewBuildInfoMetric creates an initialised BuildInfoMetric instance ready to be registered.
func NewBuildInfoMetric() *BuildInfoMetric {
	return &BuildInfoMetric{
		Info: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "build_info",
			Help:      "A metric with a constant value of 1 that exposes build and version information for the operator.",
		}, []string{"version", "goversion"}),
	}
}

// Set reads build metadata at runtime and sets the info gauge to 1.
// Call this once at startup after the metric is registered.
func (b *BuildInfoMetric) Set() {
	goVersion := runtime.Version()
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	b.Info.WithLabelValues(version, goVersion).Set(1)
}
