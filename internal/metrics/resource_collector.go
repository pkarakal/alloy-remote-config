// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package metrics

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

// ResourceCollector is a prometheus.Collector that queries the controller-runtime
// informer cache on each scrape to report current fleet resource state.
// Reading from the cache is O(1) in-memory, so scrape overhead is negligible.
type ResourceCollector struct {
	reader    client.Reader
	namespace string

	descPipelineConfigs        *prometheus.Desc
	descCollectorGroupBindings *prometheus.Desc
	descCollectorGroups        *prometheus.Desc
	descDeletionBlocked        *prometheus.Desc

	// ScrapeErrors counts List failures during Collect, partitioned by resource kind.
	// Registered separately (not emitted by this Collector) so it increments monotonically.
	ScrapeErrors *prometheus.CounterVec
}

// NewResourceCollector creates a ResourceCollector backed by the given informer cache reader.
func NewResourceCollector(reader client.Reader, namespace string) *ResourceCollector {
	constLabels := prometheus.Labels{"namespace": namespace}
	return &ResourceCollector{
		reader:    reader,
		namespace: namespace,

		// Gauge names do not carry the _total suffix — that suffix is reserved for counters.
		descPipelineConfigs: prometheus.NewDesc(
			metricsNamespace+"_pipeline_configs",
			"Current number of PipelineConfig resources, partitioned by content validity.",
			[]string{"validity"}, constLabels,
		),
		descCollectorGroupBindings: prometheus.NewDesc(
			metricsNamespace+"_collector_group_bindings",
			"Current number of CollectorGroupBinding resources, partitioned by phase.",
			[]string{"phase"}, constLabels,
		),
		descCollectorGroups: prometheus.NewDesc(
			metricsNamespace+"_collector_groups",
			"Current total number of CollectorGroup resources.",
			nil, constLabels,
		),
		descDeletionBlocked: prometheus.NewDesc(
			metricsNamespace+"_deletion_blocked_resources",
			"Current number of resources that have a deletion timestamp set but are blocked because activeBindings > 0.",
			[]string{"kind"}, constLabels,
		),

		ScrapeErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Name:      "resource_scrape_errors_total",
				Help:      "Total number of errors encountered while listing resources from the informer cache during a metrics scrape, partitioned by resource kind.",
			},
			[]string{"kind"},
		),
	}
}

// Describe sends all metric descriptors to ch.
func (rc *ResourceCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- rc.descPipelineConfigs
	ch <- rc.descCollectorGroupBindings
	ch <- rc.descCollectorGroups
	ch <- rc.descDeletionBlocked
}

// Collect queries the informer cache and emits current resource state as gauge metrics.
// List errors increment ScrapeErrors and skip that metric so that a single unavailable
// resource type does not suppress the rest.
func (rc *ResourceCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()
	log := logf.Log.WithName("metrics.resource-collector")

	rc.collectPipelineConfigs(ctx, log, ch)
	rc.collectCollectorGroupBindings(ctx, log, ch)
	rc.collectCollectorGroups(ctx, log, ch)
	rc.collectDeletionBlocked(ctx, log, ch)
}

func (rc *ResourceCollector) collectPipelineConfigs(ctx context.Context, log logr, ch chan<- prometheus.Metric) {
	var list fleetv1alpha1.PipelineConfigList
	if err := rc.reader.List(ctx, &list, client.InNamespace(rc.namespace)); err != nil {
		log.Error(err, "Failed to list PipelineConfigs for metrics collection")
		rc.ScrapeErrors.WithLabelValues("pipeline_config").Inc()
		return
	}

	var valid, invalid float64
	for i := range list.Items {
		if isConditionTrue(list.Items[i].Status.Conditions, "ContentValid") {
			valid++
		} else {
			invalid++
		}
	}
	ch <- prometheus.MustNewConstMetric(rc.descPipelineConfigs, prometheus.GaugeValue, valid, "valid")
	ch <- prometheus.MustNewConstMetric(rc.descPipelineConfigs, prometheus.GaugeValue, invalid, "invalid")
}

func (rc *ResourceCollector) collectCollectorGroupBindings(ctx context.Context, log logr, ch chan<- prometheus.Metric) {
	var list fleetv1alpha1.CollectorGroupBindingList
	if err := rc.reader.List(ctx, &list, client.InNamespace(rc.namespace)); err != nil {
		log.Error(err, "Failed to list CollectorGroupBindings for metrics collection")
		rc.ScrapeErrors.WithLabelValues("collector_group_binding").Inc()
		return
	}

	// Pre-seed all known phases so zero-value series are always emitted.
	// Label values are normalised to lowercase for consistency with all other label values.
	counts := map[string]float64{
		"pending":  0,
		"active":   0,
		"degraded": 0,
	}
	for i := range list.Items {
		phase := strings.ToLower(string(list.Items[i].Status.Phase))
		if phase == "" {
			phase = "pending"
		}
		counts[phase]++
	}
	for phase, count := range counts {
		ch <- prometheus.MustNewConstMetric(rc.descCollectorGroupBindings, prometheus.GaugeValue, count, phase)
	}
}

func (rc *ResourceCollector) collectCollectorGroups(ctx context.Context, log logr, ch chan<- prometheus.Metric) {
	var list fleetv1alpha1.CollectorGroupList
	if err := rc.reader.List(ctx, &list, client.InNamespace(rc.namespace)); err != nil {
		log.Error(err, "Failed to list CollectorGroups for metrics collection")
		rc.ScrapeErrors.WithLabelValues("collector_group").Inc()
		return
	}
	ch <- prometheus.MustNewConstMetric(rc.descCollectorGroups, prometheus.GaugeValue, float64(len(list.Items)))
}

func (rc *ResourceCollector) collectDeletionBlocked(ctx context.Context, log logr, ch chan<- prometheus.Metric) {
	var blockedPC, blockedCG float64

	var pcList fleetv1alpha1.PipelineConfigList
	if err := rc.reader.List(ctx, &pcList, client.InNamespace(rc.namespace)); err != nil {
		log.Error(err, "Failed to list PipelineConfigs for deletion-blocked metrics collection")
		rc.ScrapeErrors.WithLabelValues("pipeline_config").Inc()
	} else {
		for i := range pcList.Items {
			if pcList.Items[i].DeletionTimestamp != nil && pcList.Items[i].Status.ActiveBindings > 0 {
				blockedPC++
			}
		}
	}

	var cgList fleetv1alpha1.CollectorGroupList
	if err := rc.reader.List(ctx, &cgList, client.InNamespace(rc.namespace)); err != nil {
		log.Error(err, "Failed to list CollectorGroups for deletion-blocked metrics collection")
		rc.ScrapeErrors.WithLabelValues("collector_group").Inc()
	} else {
		for i := range cgList.Items {
			if cgList.Items[i].DeletionTimestamp != nil && cgList.Items[i].Status.ActiveBindings > 0 {
				blockedCG++
			}
		}
	}

	// kind label values use snake_case to match Prometheus conventions.
	ch <- prometheus.MustNewConstMetric(rc.descDeletionBlocked, prometheus.GaugeValue, blockedPC, "pipeline_config")
	ch <- prometheus.MustNewConstMetric(rc.descDeletionBlocked, prometheus.GaugeValue, blockedCG, "collector_group")
}

// isConditionTrue returns true if conditions contains a condition of the given type with status True.
func isConditionTrue(conditions []metav1.Condition, condType string) bool {
	for _, c := range conditions {
		if c.Type == condType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// logr is a local alias for the go-logr Logger interface used in helper methods.
type logr = interface {
	Error(err error, msg string, keysAndValues ...any)
}
