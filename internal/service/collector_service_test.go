// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pkarakal/alloy-remote-config/internal/metrics"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

// --- Fakes ---

type fakeResolver struct {
	byTenant         map[string]*port.ResolvedConfig
	byCollectorGroup map[string]*port.ResolvedConfig
	defaultConfig    *port.ResolvedConfig
	err              error
}

// Resolve emulates the real resolver's outcome: tenant, then group, then default.
func (f *fakeResolver) Resolve(_ context.Context, _ string, attrs map[string]string) (*port.ResolvedConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	if rc, ok := f.byTenant[attrs["tenant"]]; ok && attrs["tenant"] != "" {
		return rc, nil
	}
	if rc, ok := f.byCollectorGroup[attrs["collector_group"]]; ok && attrs["collector_group"] != "" {
		return rc, nil
	}
	if f.defaultConfig != nil {
		return f.defaultConfig, nil
	}
	return nil, port.ErrConfigNotFound
}

type fakeRegistry struct {
	registerErr   error
	heartbeatErr  error
	unregisterErr error
	registers     int
	heartbeats    int
}

func (f *fakeRegistry) Register(_ context.Context, _ port.CollectorInfo) error {
	f.registers++
	return f.registerErr
}

func (f *fakeRegistry) Heartbeat(_ context.Context, _ port.CollectorInfo) error {
	f.heartbeats++
	return f.heartbeatErr
}

func (f *fakeRegistry) Unregister(_ context.Context, _ string) error {
	return f.unregisterErr
}

// --- Helpers ---

func newGetConfigRequest(hash string, attrs map[string]string) *connect.Request[collectorv1.GetConfigRequest] {
	return connect.NewRequest(&collectorv1.GetConfigRequest{
		Id:              "test-collector",
		Hash:            hash,
		LocalAttributes: attrs,
	})
}

func connectErrCode(err error) connect.Code {
	var connectErr *connect.Error
	Expect(errors.As(err, &connectErr)).To(BeTrue(), "expected a connect.Error")
	return connectErr.Code()
}

// --- Suite ---

func TestCollectorService(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "CollectorService Suite")
}

var _ = Describe("CollectorService", func() {
	ctx := context.Background()

	Context("GetConfig", func() {
		It("should resolve config by tenant", func() {
			resolver := &fakeResolver{
				byTenant: map[string]*port.ResolvedConfig{
					"tenant-a": {Content: "tenant config", ContentHash: "abc123"},
				},
			}
			svc := NewCollectorService(resolver, &fakeRegistry{}, "default")

			resp, err := svc.GetConfig(ctx, newGetConfigRequest("", map[string]string{
				"tenant": "tenant-a",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Content).To(Equal("tenant config"))
			Expect(resp.Msg.Hash).To(Equal("abc123"))
		})

		It("should fall through from tenant to collector group", func() {
			resolver := &fakeResolver{
				byTenant: map[string]*port.ResolvedConfig{},
				byCollectorGroup: map[string]*port.ResolvedConfig{
					"group-1": {Content: "group config", ContentHash: "def456"},
				},
			}
			svc := NewCollectorService(resolver, &fakeRegistry{}, "default")

			resp, err := svc.GetConfig(ctx, newGetConfigRequest("", map[string]string{
				"tenant":          "unknown-tenant",
				"collector_group": "group-1",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Content).To(Equal("group config"))
		})

		It("should fall through to default", func() {
			resolver := &fakeResolver{
				byTenant:         map[string]*port.ResolvedConfig{},
				byCollectorGroup: map[string]*port.ResolvedConfig{},
				defaultConfig:    &port.ResolvedConfig{Content: "default config", ContentHash: "ghi789"},
			}
			svc := NewCollectorService(resolver, &fakeRegistry{}, "default")

			resp, err := svc.GetConfig(ctx, newGetConfigRequest("", map[string]string{
				"tenant":          "unknown",
				"collector_group": "unknown",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Content).To(Equal("default config"))
		})

		It("should return NotModified when hash matches", func() {
			resolver := &fakeResolver{
				defaultConfig: &port.ResolvedConfig{Content: "some config", ContentHash: "same-hash"},
			}
			svc := NewCollectorService(resolver, &fakeRegistry{}, "default")

			resp, err := svc.GetConfig(ctx, newGetConfigRequest("same-hash", nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.NotModified).To(BeTrue())
			Expect(resp.Msg.Content).To(BeEmpty())
		})

		It("should return CodeNotFound when no config exists", func() {
			resolver := &fakeResolver{}
			svc := NewCollectorService(resolver, &fakeRegistry{}, "default")

			_, err := svc.GetConfig(ctx, newGetConfigRequest("", nil))
			Expect(err).To(HaveOccurred())
			Expect(connectErrCode(err)).To(Equal(connect.CodeNotFound))
		})

		It("should return CodeUnavailable when config is not yet reconciled", func() {
			resolver := &fakeResolver{
				err: port.ErrConfigNotReady,
			}
			svc := NewCollectorService(resolver, &fakeRegistry{}, "default")

			_, err := svc.GetConfig(ctx, newGetConfigRequest("", map[string]string{
				"tenant": "some-tenant",
			}))
			Expect(err).To(HaveOccurred())
			Expect(connectErrCode(err)).To(Equal(connect.CodeUnavailable))
		})

		It("should return CodeInternal on unexpected errors", func() {
			resolver := &fakeResolver{
				err: errors.New("database connection failed"),
			}
			svc := NewCollectorService(resolver, &fakeRegistry{}, "default")

			_, err := svc.GetConfig(ctx, newGetConfigRequest("", map[string]string{
				"tenant": "t",
			}))
			Expect(err).To(HaveOccurred())
			Expect(connectErrCode(err)).To(Equal(connect.CodeInternal))
		})

		It("should give tenant precedence over collector group", func() {
			resolver := &fakeResolver{
				byTenant: map[string]*port.ResolvedConfig{
					"t1": {Content: "tenant wins", ContentHash: "t-hash"},
				},
				byCollectorGroup: map[string]*port.ResolvedConfig{
					"g1": {Content: "group loses", ContentHash: "g-hash"},
				},
			}
			svc := NewCollectorService(resolver, &fakeRegistry{}, "default")

			resp, err := svc.GetConfig(ctx, newGetConfigRequest("", map[string]string{
				"tenant":          "t1",
				"collector_group": "g1",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Content).To(Equal("tenant wins"))
		})

		It("should succeed even when the registration refresh fails", func() {
			resolver := &fakeResolver{
				defaultConfig: &port.ResolvedConfig{Content: "default config", ContentHash: "hash"},
			}
			svc := NewCollectorService(resolver, &fakeRegistry{
				heartbeatErr: errors.New("registry unavailable"),
			}, "default")

			resp, err := svc.GetConfig(ctx, newGetConfigRequest("", nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Content).To(Equal("default config"))
		})

		It("should heartbeat rather than register on each poll", func() {
			resolver := &fakeResolver{
				defaultConfig: &port.ResolvedConfig{Content: "c", ContentHash: "h"},
			}
			reg := &fakeRegistry{}
			svc := NewCollectorService(resolver, reg, "default")

			_, err := svc.GetConfig(ctx, newGetConfigRequest("", nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(reg.heartbeats).To(Equal(1))
			Expect(reg.registers).To(BeZero())
		})
	})

	Context("RegisterCollector", func() {
		It("should succeed with a no-op registry", func() {
			svc := NewCollectorService(&fakeResolver{}, &fakeRegistry{}, "default")

			resp, err := svc.RegisterCollector(ctx, connect.NewRequest(&collectorv1.RegisterCollectorRequest{
				Id:   "c1",
				Name: "collector-1",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg).NotTo(BeNil())
		})

		It("should return OK and count the error when the registry fails", func() {
			m := metrics.NewRPCMetrics()
			svc := NewCollectorService(&fakeResolver{}, &fakeRegistry{
				registerErr: errors.New("registry failure"),
			}, "default", m)

			resp, err := svc.RegisterCollector(ctx, connect.NewRequest(&collectorv1.RegisterCollectorRequest{
				Id: "c1",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg).NotTo(BeNil())
			Expect(testutil.ToFloat64(m.RegistryErrors.WithLabelValues("register"))).To(Equal(1.0))
		})

		It("should return OK for a collector without a tenant attribute", func() {
			svc := NewCollectorService(&fakeResolver{}, &fakeRegistry{}, "default")

			resp, err := svc.RegisterCollector(ctx, connect.NewRequest(&collectorv1.RegisterCollectorRequest{
				Id:              "c1",
				LocalAttributes: map[string]string{"collector_group": "prod"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg).NotTo(BeNil())
		})
	})

	Context("UnregisterCollector", func() {
		It("should succeed with a no-op registry", func() {
			svc := NewCollectorService(&fakeResolver{}, &fakeRegistry{}, "default")

			resp, err := svc.UnregisterCollector(ctx, connect.NewRequest(&collectorv1.UnregisterCollectorRequest{
				Id: "c1",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg).NotTo(BeNil())
		})

		It("should return OK and count the error when the registry fails", func() {
			m := metrics.NewRPCMetrics()
			svc := NewCollectorService(&fakeResolver{}, &fakeRegistry{
				unregisterErr: errors.New("registry failure"),
			}, "default", m)

			resp, err := svc.UnregisterCollector(ctx, connect.NewRequest(&collectorv1.UnregisterCollectorRequest{
				Id: "c1",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg).NotTo(BeNil())
			Expect(testutil.ToFloat64(m.RegistryErrors.WithLabelValues("unregister"))).To(Equal(1.0))
		})
	})
})
