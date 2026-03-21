// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package interceptor

import (
	"context"
	"path"
	"time"

	"connectrpc.com/connect"

	"github.com/pkarakal/alloy-remote-config/internal/metrics"
)

// NewMetricsInterceptor returns a Connect-RPC interceptor that records request
// count and latency for every unary RPC call. It tracks:
//   - alloy_remote_config_rpc_requests_total{procedure, code}
//   - alloy_remote_config_rpc_request_duration_seconds{procedure}
//
// The procedure label is the short method name (e.g. "GetConfig") trimmed from
// the full Connect procedure path.
func NewMetricsInterceptor(m *metrics.RPCMetrics) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			procedure := path.Base(req.Spec().Procedure)
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
			m.Duration.WithLabelValues(procedure).Observe(elapsed)

			return resp, err
		}
	})
}
