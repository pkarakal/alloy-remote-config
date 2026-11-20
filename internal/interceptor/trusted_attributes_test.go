// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package interceptor_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	"github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1/collectorv1connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pkarakal/alloy-remote-config/internal/interceptor"
	"github.com/pkarakal/alloy-remote-config/internal/metrics"
	"github.com/pkarakal/alloy-remote-config/internal/port"
	"github.com/pkarakal/alloy-remote-config/internal/service"
)

func TestInterceptor(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Interceptor Suite")
}

var tenantKey = []interceptor.TrustedAttribute{{Key: "tenant", Header: "X-Scope-OrgID"}}

type outcome struct {
	called bool
	attrs  map[string]string
	scope  port.UnregisterScope
	code   connect.Code // 0 when no error
}

// run sends the message through the interceptor with a recording next handler.
func run(trusted []interceptor.TrustedAttribute, m *metrics.RPCMetrics, req connect.AnyRequest, attrs func() map[string]string) outcome {
	var out outcome
	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out.called = true
		out.attrs = attrs()
		out.scope = port.UnregisterScopeFromContext(ctx)
		return connect.NewResponse(&collectorv1.GetConfigResponse{}), nil
	}
	_, err := interceptor.NewTrustedAttributesInterceptor(trusted, m).WrapUnary(next)(context.Background(), req)
	if err != nil {
		out.code = connect.CodeOf(err)
	}
	return out
}

func withHeaders(r connect.AnyRequest, headers map[string][]string) {
	for k, vs := range headers {
		for _, v := range vs {
			r.Header().Add(k, v)
		}
	}
}

var _ = Describe("TrustedAttributesInterceptor", func() {
	DescribeTable("GetConfig",
		func(trusted []interceptor.TrustedAttribute, headers map[string][]string, attrs map[string]string,
			wantCode connect.Code, wantReason string, wantAttrs map[string]string) {
			m := metrics.NewRPCMetrics()
			req := connect.NewRequest(&collectorv1.GetConfigRequest{Id: "c1", LocalAttributes: attrs})
			withHeaders(req, headers)

			out := run(trusted, m, req, func() map[string]string { return req.Msg.LocalAttributes })

			Expect(out.code).To(Equal(wantCode))
			if wantCode != 0 {
				Expect(out.called).To(BeFalse())
				Expect(testutil.ToFloat64(m.TrustedAttrRejections.WithLabelValues("GetConfig", wantReason))).To(Equal(1.0))
				return
			}
			Expect(out.called).To(BeTrue())
			Expect(out.attrs).To(Equal(wantAttrs))
		},
		Entry("no trusted keys passes through untouched", nil, nil,
			map[string]string{"tenant": "evil"}, connect.Code(0), "", map[string]string{"tenant": "evil"}),
		Entry("header missing", tenantKey, nil,
			map[string]string{"tenant": "acme"}, connect.CodeUnauthenticated, "missing", nil),
		Entry("header empty", tenantKey, map[string][]string{"X-Scope-OrgID": {""}},
			map[string]string{"tenant": "acme"}, connect.CodeUnauthenticated, "missing", nil),
		Entry("header ambiguous", tenantKey, map[string][]string{"X-Scope-OrgID": {"acme", "other"}},
			map[string]string{"tenant": "acme"}, connect.CodeUnauthenticated, "ambiguous", nil),
		Entry("repeated identical header is not ambiguous", tenantKey, map[string][]string{"X-Scope-OrgID": {"acme", "acme"}},
			nil, connect.Code(0), "", map[string]string{"tenant": "acme"}),
		Entry("attribute mismatch", tenantKey, map[string][]string{"X-Scope-OrgID": {"acme"}},
			map[string]string{"tenant": "evil"}, connect.CodePermissionDenied, "mismatch", nil),
		Entry("attribute absent is overlaid", tenantKey, map[string][]string{"X-Scope-OrgID": {"acme"}},
			map[string]string{"cluster_id": "c1"}, connect.Code(0), "",
			map[string]string{"tenant": "acme", "cluster_id": "c1"}),
		Entry("equal attribute passes", tenantKey, map[string][]string{"X-Scope-OrgID": {"acme"}},
			map[string]string{"tenant": "acme"}, connect.Code(0), "", map[string]string{"tenant": "acme"}),
		Entry("two trusted keys overlay both",
			[]interceptor.TrustedAttribute{{Key: "tenant", Header: "X-Scope-OrgID"}, {Key: "org_id", Header: "X-Org-ID"}},
			map[string][]string{"X-Scope-OrgID": {"acme"}, "X-Org-ID": {"o1"}},
			map[string]string{"tenant": "acme"}, connect.Code(0), "",
			map[string]string{"tenant": "acme", "org_id": "o1"}),
		Entry("two trusted keys, second mismatches",
			[]interceptor.TrustedAttribute{{Key: "tenant", Header: "X-Scope-OrgID"}, {Key: "org_id", Header: "X-Org-ID"}},
			map[string][]string{"X-Scope-OrgID": {"acme"}, "X-Org-ID": {"o1"}},
			map[string]string{"tenant": "acme", "org_id": "o2"}, connect.CodePermissionDenied, "mismatch", nil),
	)

	DescribeTable("RegisterCollector never fails",
		func(headers map[string][]string, attrs map[string]string, wantSkipped bool, wantReason string, wantAttrs map[string]string) {
			m := metrics.NewRPCMetrics()
			req := connect.NewRequest(&collectorv1.RegisterCollectorRequest{Id: "c1", LocalAttributes: attrs})
			withHeaders(req, headers)

			out := run(tenantKey, m, req, func() map[string]string { return req.Msg.LocalAttributes })

			Expect(out.code).To(Equal(connect.Code(0)))
			Expect(out.called).To(Equal(!wantSkipped))
			if wantSkipped {
				Expect(testutil.ToFloat64(m.TrustedAttrRejections.WithLabelValues("RegisterCollector", wantReason))).To(Equal(1.0))
				return
			}
			Expect(out.attrs).To(Equal(wantAttrs))
		},
		Entry("header missing", nil, map[string]string{"tenant": "acme"}, true, "missing", nil),
		Entry("header ambiguous", map[string][]string{"X-Scope-OrgID": {"a", "b"}}, nil, true, "ambiguous", nil),
		Entry("mismatch", map[string][]string{"X-Scope-OrgID": {"acme"}}, map[string]string{"tenant": "evil"}, true, "mismatch", nil),
		Entry("absent is overlaid", map[string][]string{"X-Scope-OrgID": {"acme"}}, nil, false, "", map[string]string{"tenant": "acme"}),
		Entry("equal passes", map[string][]string{"X-Scope-OrgID": {"acme"}}, map[string]string{"tenant": "acme"}, false, "",
			map[string]string{"tenant": "acme"}),
	)

	DescribeTable("UnregisterCollector",
		func(trusted []interceptor.TrustedAttribute, headers map[string][]string, wantCalled bool, wantScope port.UnregisterScope) {
			req := connect.NewRequest(&collectorv1.UnregisterCollectorRequest{Id: "c1"})
			withHeaders(req, headers)

			out := run(trusted, nil, req, func() map[string]string { return nil })

			Expect(out.code).To(Equal(connect.Code(0)))
			Expect(out.called).To(Equal(wantCalled))
			Expect(out.scope).To(Equal(wantScope))
		},
		Entry("tenant header scopes the removal", tenantKey, map[string][]string{"X-Scope-OrgID": {"acme"}},
			true, port.UnregisterScope{Tenant: "acme"}),
		Entry("header missing is skipped", tenantKey, nil, false, port.UnregisterScope{}),
		Entry("tenant not trusted is unscoped",
			[]interceptor.TrustedAttribute{{Key: "org_id", Header: "X-Org-ID"}}, nil, true, port.UnregisterScope{}),
	)

	It("does not mutate the map the client message came with", func() {
		original := map[string]string{"cluster_id": "c1"}
		req := connect.NewRequest(&collectorv1.GetConfigRequest{LocalAttributes: original})
		req.Header().Set("X-Scope-OrgID", "acme")

		run(tenantKey, nil, req, func() map[string]string { return nil })

		Expect(original).To(Equal(map[string]string{"cluster_id": "c1"}))
	})

	It("is nil-safe without metrics", func() {
		req := connect.NewRequest(&collectorv1.GetConfigRequest{})
		out := run(tenantKey, nil, req, func() map[string]string { return nil })
		Expect(out.code).To(Equal(connect.CodeUnauthenticated))
	})
})

// recordingResolver returns a config named after the tenant it was asked for.
type recordingResolver struct{ tenants []string }

func (r *recordingResolver) Resolve(_ context.Context, _ string, attrs map[string]string) (*port.ResolvedConfig, error) {
	r.tenants = append(r.tenants, attrs["tenant"])
	return &port.ResolvedConfig{Content: "cfg-" + attrs["tenant"], ContentHash: "h-" + attrs["tenant"]}, nil
}

type recordingRegistry struct {
	registered []port.CollectorInfo
	heartbeats []port.CollectorInfo
	scopes     []port.UnregisterScope
}

func (r *recordingRegistry) Register(_ context.Context, i port.CollectorInfo) error {
	r.registered = append(r.registered, i)
	return nil
}

func (r *recordingRegistry) Heartbeat(_ context.Context, i port.CollectorInfo) error {
	r.heartbeats = append(r.heartbeats, i)
	return nil
}

func (r *recordingRegistry) Unregister(_ context.Context, _ string, s port.UnregisterScope) error {
	r.scopes = append(r.scopes, s)
	return nil
}

var _ = Describe("TrustedAttributesInterceptor through a Connect handler", func() {
	var (
		resolver *recordingResolver
		registry *recordingRegistry
		srv      *httptest.Server
		client   collectorv1connect.CollectorServiceClient
		header   string
	)

	BeforeEach(func() {
		resolver, registry, header = &recordingResolver{}, &recordingRegistry{}, ""
		svc := service.NewCollectorService(resolver, registry, "default")
		mux := http.NewServeMux()
		mux.Handle(collectorv1connect.NewCollectorServiceHandler(svc,
			connect.WithInterceptors(interceptor.NewTrustedAttributesInterceptor(tenantKey, nil))))
		srv = httptest.NewServer(mux)
		DeferCleanup(srv.Close)
		client = collectorv1connect.NewCollectorServiceClient(srv.Client(), srv.URL)
	})

	withOrg := func(r interface{ Header() http.Header }) {
		if header != "" {
			r.Header().Set("X-Scope-OrgID", header)
		}
	}

	It("resolves with the header tenant when the client sent none", func() {
		header = "acme"
		req := connect.NewRequest(&collectorv1.GetConfigRequest{Id: "c1", LocalAttributes: map[string]string{"cluster_id": "x"}})
		withOrg(req)

		resp, err := client.GetConfig(context.Background(), req)

		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetContent()).To(Equal("cfg-acme"))
		Expect(resolver.tenants).To(Equal([]string{"acme"}))
		Expect(registry.heartbeats).To(HaveLen(1))
		Expect(registry.heartbeats[0].LocalAttributes).To(HaveKeyWithValue("tenant", "acme"))
	})

	It("refuses a spoofed tenant without reaching the resolver", func() {
		header = "acme"
		req := connect.NewRequest(&collectorv1.GetConfigRequest{Id: "c1", LocalAttributes: map[string]string{"tenant": "victim"}})
		withOrg(req)

		_, err := client.GetConfig(context.Background(), req)

		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(resolver.tenants).To(BeEmpty())
	})

	It("answers OK to a Register without the header and skips the registry", func() {
		_, err := client.RegisterCollector(context.Background(),
			connect.NewRequest(&collectorv1.RegisterCollectorRequest{Id: "c1"}))

		Expect(err).NotTo(HaveOccurred())
		Expect(registry.registered).To(BeEmpty())
	})

	It("scopes Unregister to the header tenant", func() {
		header = "acme"
		req := connect.NewRequest(&collectorv1.UnregisterCollectorRequest{Id: "c1"})
		withOrg(req)

		_, err := client.UnregisterCollector(context.Background(), req)

		Expect(err).NotTo(HaveOccurred())
		Expect(registry.scopes).To(Equal([]port.UnregisterScope{{Tenant: "acme"}}))
	})
})

var _ = DescribeTable("ParseTrustedAttribute",
	func(spec string, want interceptor.TrustedAttribute, wantErr bool) {
		got, err := interceptor.ParseTrustedAttribute(spec)
		if wantErr {
			Expect(err).To(HaveOccurred())
			return
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(want))
	},
	Entry("valid", "tenant=X-Scope-OrgID", interceptor.TrustedAttribute{Key: "tenant", Header: "X-Scope-OrgID"}, false),
	Entry("missing equals", "tenant", interceptor.TrustedAttribute{}, true),
	Entry("empty key", "=X-Org", interceptor.TrustedAttribute{}, true),
	Entry("empty header", "tenant=", interceptor.TrustedAttribute{}, true),
	Entry("bad header token", "tenant=X Org", interceptor.TrustedAttribute{}, true),
	Entry("header with colon", "tenant=X-Org:", interceptor.TrustedAttribute{}, true),
)

var _ = Describe("TrustedAttributeFlag", func() {
	It("rejects a duplicate key", func() {
		var f interceptor.TrustedAttributeFlag
		Expect(f.Set("tenant=X-A")).To(Succeed())
		Expect(f.Set("org_id=X-B")).To(Succeed())
		Expect(f.Set("tenant=X-C")).To(MatchError(ContainSubstring("more than once")))
		Expect(f).To(HaveLen(2))
		Expect(f.String()).To(Equal("tenant=X-A,org_id=X-B"))
	})
})
