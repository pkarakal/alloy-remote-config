// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package interceptor

import (
	"context"
	"time"

	"connectrpc.com/connect"

	"github.com/pkarakal/alloy-remote-config/internal/metrics"
)

// NewMetricsInterceptor returns a Connect-RPC interceptor that records request
// count and latency for every unary RPC call. It tracks:
//   - alloy_remote_config_rpc_requests_total{procedure, code}
//   - alloy_remote_config_rpc_request_duration_seconds{procedure, code}
//
// The procedure label is the full Connect procedure path
// (e.g. "/collector.v1.CollectorService/GetConfig").
func NewMetricsInterceptor(m *metrics.RPCMetrics) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			procedure := req.Spec().Procedure
			start := time.Now()

			resp, err := next(ctx, req)

			elapsed := time.Since(start).Seconds()
			// connect's codeToString map omits CodeOK (0), so Code(0).String() returns
			// "code_0" rather than "ok". Handle the success case explicitly.
			var code string
			if err == nil {
				code = "ok"
			} else {
				code = connect.CodeOf(err).String()
			}

			m.Requests.WithLabelValues(procedure, code).Inc()
			m.Duration.WithLabelValues(procedure, code).Observe(elapsed)

			return resp, err
		}
	})
}
