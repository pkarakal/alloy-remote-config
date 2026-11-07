// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pkarakal/alloy-remote-config/internal/metrics"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

type stubResolver struct{ res *port.ResolvedConfig }

func (s stubResolver) Resolve(context.Context, string, map[string]string) (*port.ResolvedConfig, error) {
	return s.res, nil
}

var _ = Describe("MetricsConfigResolver additive histogram", func() {
	It("observes the number of composed additives per resolved config", func() {
		m := metrics.NewResolutionMetrics()
		r := NewMetricsConfigResolver(stubResolver{&port.ResolvedConfig{
			Path: port.PathTenant, Additives: []string{"a", "b"},
		}}, m)
		_, err := r.Resolve(context.Background(), "ns", nil)
		Expect(err).NotTo(HaveOccurred())

		const want = `
# HELP alloy_remote_config_resolution_additive_pipelines Number of additive pipelines composed into each resolved config.
# TYPE alloy_remote_config_resolution_additive_pipelines histogram
alloy_remote_config_resolution_additive_pipelines_bucket{le="0"} 0
alloy_remote_config_resolution_additive_pipelines_bucket{le="1"} 0
alloy_remote_config_resolution_additive_pipelines_bucket{le="2"} 1
alloy_remote_config_resolution_additive_pipelines_bucket{le="3"} 1
alloy_remote_config_resolution_additive_pipelines_bucket{le="5"} 1
alloy_remote_config_resolution_additive_pipelines_bucket{le="8"} 1
alloy_remote_config_resolution_additive_pipelines_bucket{le="+Inf"} 1
alloy_remote_config_resolution_additive_pipelines_sum 2
alloy_remote_config_resolution_additive_pipelines_count 1
`
		Expect(testutil.CollectAndCompare(m.AdditivePipelines, strings.NewReader(want))).To(Succeed())
	})
})
