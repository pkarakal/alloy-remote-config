// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package metrics

import "github.com/prometheus/client_golang/prometheus"

// Register registers all custom operator metrics against the given registerer.
// Pass ctrlmetrics.Registry (from sigs.k8s.io/controller-runtime/pkg/metrics) to
// surface metrics on the existing controller-manager /metrics endpoint.
func Register(reg prometheus.Registerer, rpc *RPCMetrics, res *ResolutionMetrics, rc *ResourceCollector, bi *BuildInfoMetric, ctrl *ControllerMetrics, httpM *HTTPMetrics) error {
	collectors := []prometheus.Collector{
		rpc.Requests,
		rpc.Duration,
		rpc.Served,
		rpc.NotModified,
		res.Total,
		res.Duration,
		rc,
		rc.ScrapeErrors,
		bi.Info,
		ctrl.TenantsEvicted,
		httpM.RequestsTotal,
		httpM.RequestDuration,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}
