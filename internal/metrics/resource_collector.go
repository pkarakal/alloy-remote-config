// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package metrics

import (
	"context"
	"strings"

	"github.com/go-logr/logr"
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
	log       logr.Logger

	descPipelineConfigs        *prometheus.Desc
	descCollectorGroupBindings *prometheus.Desc
	descCollectorGroups        *prometheus.Desc
	descDeletionBlocked        *prometheus.Desc
	descRegisteredTenants      *prometheus.Desc

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
		log:       logf.Log.WithName("metrics.resource-collector"),

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
			"Current number of CollectorGroup resources, partitioned by binding activity.",
			[]string{"bindings"}, constLabels,
		),
		descDeletionBlocked: prometheus.NewDesc(
			metricsNamespace+"_deletion_blocked_resources",
			"Current number of resources that have a deletion timestamp set but are blocked because activeBindings > 0.",
			[]string{"kind"}, constLabels,
		),
		descRegisteredTenants: prometheus.NewDesc(
			metricsNamespace+"_registered_tenants",
			"Current total number of registered tenant entries across all CollectorGroupBinding resources.",
			nil, constLabels,
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
	ch <- rc.descRegisteredTenants
}

// Collect queries the informer cache and emits current resource state as gauge metrics.
// List errors increment ScrapeErrors and skip that metric so that a single unavailable
// resource type does not suppress the rest.
// PipelineConfig and CollectorGroup lists are fetched once and reused by collectDeletionBlocked
// to avoid double-listing and intra-scrape inconsistency.
func (rc *ResourceCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()

	pcList := rc.collectPipelineConfigs(ctx, ch)
	rc.collectCollectorGroupBindings(ctx, ch)
	cgList := rc.collectCollectorGroups(ctx, ch)
	rc.collectDeletionBlocked(ch, pcList, cgList)
	rc.collectRegisteredTenants(ctx, ch)
}

func (rc *ResourceCollector) collectPipelineConfigs(ctx context.Context, ch chan<- prometheus.Metric) *fleetv1alpha1.PipelineConfigList {
	var list fleetv1alpha1.PipelineConfigList
	if err := rc.reader.List(ctx, &list, client.InNamespace(rc.namespace)); err != nil {
		rc.log.Error(err, "Failed to list PipelineConfigs for metrics collection")
		rc.ScrapeErrors.WithLabelValues("pipeline_config").Inc()
		return nil
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
	return &list
}

func (rc *ResourceCollector) collectCollectorGroupBindings(ctx context.Context, ch chan<- prometheus.Metric) {
	var list fleetv1alpha1.CollectorGroupBindingList
	if err := rc.reader.List(ctx, &list, client.InNamespace(rc.namespace)); err != nil {
		rc.log.Error(err, "Failed to list CollectorGroupBindings for metrics collection")
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

func (rc *ResourceCollector) collectCollectorGroups(ctx context.Context, ch chan<- prometheus.Metric) *fleetv1alpha1.CollectorGroupList {
	var list fleetv1alpha1.CollectorGroupList
	if err := rc.reader.List(ctx, &list, client.InNamespace(rc.namespace)); err != nil {
		rc.log.Error(err, "Failed to list CollectorGroups for metrics collection")
		rc.ScrapeErrors.WithLabelValues("collector_group").Inc()
		return nil
	}
	var active, inactive float64
	for i := range list.Items {
		if list.Items[i].Status.ActiveBindings > 0 {
			active++
		} else {
			inactive++
		}
	}
	ch <- prometheus.MustNewConstMetric(rc.descCollectorGroups, prometheus.GaugeValue, active, "active")
	ch <- prometheus.MustNewConstMetric(rc.descCollectorGroups, prometheus.GaugeValue, inactive, "inactive")
	return &list
}

// collectDeletionBlocked emits the deletion-blocked gauge for each kind.
// It receives the already-fetched lists from collectPipelineConfigs and
// collectCollectorGroups to avoid re-listing in the same scrape. A nil list
// means the earlier fetch failed (already counted in ScrapeErrors); the gauge
// for that kind is skipped to preserve consistency.
func (rc *ResourceCollector) collectDeletionBlocked(
	ch chan<- prometheus.Metric,
	pcList *fleetv1alpha1.PipelineConfigList,
	cgList *fleetv1alpha1.CollectorGroupList,
) {
	// kind label values use snake_case to match Prometheus conventions.
	if pcList != nil {
		var blockedPC float64
		for i := range pcList.Items {
			if pcList.Items[i].DeletionTimestamp != nil && pcList.Items[i].Status.ActiveBindings > 0 {
				blockedPC++
			}
		}
		ch <- prometheus.MustNewConstMetric(rc.descDeletionBlocked, prometheus.GaugeValue, blockedPC, "pipeline_config")
	}

	if cgList != nil {
		var blockedCG float64
		for i := range cgList.Items {
			if cgList.Items[i].DeletionTimestamp != nil && cgList.Items[i].Status.ActiveBindings > 0 {
				blockedCG++
			}
		}
		ch <- prometheus.MustNewConstMetric(rc.descDeletionBlocked, prometheus.GaugeValue, blockedCG, "collector_group")
	}
}

func (rc *ResourceCollector) collectRegisteredTenants(ctx context.Context, ch chan<- prometheus.Metric) {
	var list fleetv1alpha1.CollectorGroupBindingList
	if err := rc.reader.List(ctx, &list, client.InNamespace(rc.namespace)); err != nil {
		rc.log.Error(err, "Failed to list CollectorGroupBindings for registered tenants metrics collection")
		rc.ScrapeErrors.WithLabelValues("collector_group_binding").Inc()
		return
	}
	var total float64
	for i := range list.Items {
		total += float64(len(list.Items[i].Status.RegisteredTenants))
	}
	ch <- prometheus.MustNewConstMetric(rc.descRegisteredTenants, prometheus.GaugeValue, total)
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
