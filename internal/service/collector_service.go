// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	"github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1/collectorv1connect"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/pkarakal/alloy-remote-config/internal/metrics"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

var _ collectorv1connect.CollectorServiceHandler = (*CollectorService)(nil)

// CollectorService implements the Connect-RPC CollectorServiceHandler by
// delegating to storage-agnostic port interfaces.
type CollectorService struct {
	resolver   port.ConfigResolver
	registry   port.CollectorRegistry
	namespace  string
	rpcMetrics *metrics.RPCMetrics
}

// NewCollectorService creates a CollectorService wired to the given ports.
// An optional *metrics.RPCMetrics can be provided to enable served/not-modified counters.
func NewCollectorService(
	resolver port.ConfigResolver,
	registry port.CollectorRegistry,
	namespace string,
	rpcMetrics ...*metrics.RPCMetrics,
) *CollectorService {
	svc := &CollectorService{
		resolver:  resolver,
		registry:  registry,
		namespace: namespace,
	}
	if len(rpcMetrics) > 0 {
		svc.rpcMetrics = rpcMetrics[0]
	}
	return svc
}

func (s *CollectorService) GetConfig(
	ctx context.Context,
	req *connect.Request[collectorv1.GetConfigRequest],
) (*connect.Response[collectorv1.GetConfigResponse], error) {
	log := logf.FromContext(ctx)
	msg := req.Msg
	tenant := msg.GetLocalAttributes()["tenant"]
	collectorGroup := msg.GetLocalAttributes()["collector_group"]

	log.V(1).Info("GetConfig called",
		"collectorID", msg.GetId(),
		"tenant", tenant,
		"collectorGroup", collectorGroup,
	)

	// Heartbeat on every poll so LastSeenAt stays current for TTL eviction;
	// Alloy only calls RegisterCollector once on startup. The registry throttles
	// the actual writes.
	if err := s.registry.Heartbeat(ctx, port.CollectorInfo{
		ID:              msg.GetId(),
		LocalAttributes: msg.GetLocalAttributes(),
	}); err != nil {
		s.countRegistryError("refresh")
		log.V(1).Info("Could not refresh collector registration during GetConfig",
			"collectorID", msg.GetId(), "err", err)
	}

	resolved, err := s.resolveConfig(ctx, msg.GetLocalAttributes())
	if err != nil {
		return nil, err
	}

	if msg.GetHash() == resolved.ContentHash {
		if s.rpcMetrics != nil {
			s.rpcMetrics.NotModified.Inc()
		}
		return connect.NewResponse(&collectorv1.GetConfigResponse{
			NotModified: true,
		}), nil
	}

	if s.rpcMetrics != nil {
		s.rpcMetrics.Served.Inc()
	}
	return connect.NewResponse(&collectorv1.GetConfigResponse{
		Content: resolved.Content,
		Hash:    resolved.ContentHash,
	}), nil
}

// resolveConfig asks the resolver for the winning candidate for these attributes.
func (s *CollectorService) resolveConfig(ctx context.Context, attrs map[string]string) (*port.ResolvedConfig, error) {
	resolved, err := s.resolver.Resolve(ctx, s.namespace, attrs)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return resolved, nil
}

// mapError converts domain errors to Connect error codes.
func (s *CollectorService) mapError(ctx context.Context, err error) *connect.Error {
	log := logf.FromContext(ctx)
	switch {
	// Expected states are polled constantly, so keep them out of the error log.
	case errors.Is(err, port.ErrConfigNotFound):
		log.V(1).Info("No config found", "reason", err.Error())
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, port.ErrConfigNotReady):
		log.V(1).Info("Config not ready", "reason", err.Error())
		return connect.NewError(connect.CodeUnavailable, err)
	default:
		log.Error(err, "Failed to resolve config")
		return connect.NewError(connect.CodeInternal, err)
	}
}

func (s *CollectorService) RegisterCollector(
	ctx context.Context,
	req *connect.Request[collectorv1.RegisterCollectorRequest],
) (*connect.Response[collectorv1.RegisterCollectorResponse], error) {
	log := logf.FromContext(ctx)
	msg := req.Msg
	if err := s.registry.Register(ctx, port.CollectorInfo{
		ID:              msg.GetId(),
		Name:            msg.GetName(),
		LocalAttributes: msg.GetLocalAttributes(),
	}); err != nil {
		// Alloy treats any RegisterCollector error as fatal for remotecfg, so
		// never fail the RPC; GetConfig keeps refreshing the registration.
		s.countRegistryError("register")
		log.Error(err, "Failed to register collector", "id", msg.GetId())
	}
	return connect.NewResponse(&collectorv1.RegisterCollectorResponse{}), nil
}

// countRegistryError increments the registry error counter when metrics are enabled.
func (s *CollectorService) countRegistryError(op string) {
	if s.rpcMetrics != nil {
		s.rpcMetrics.RegistryErrors.WithLabelValues(op).Inc()
	}
}

func (s *CollectorService) UnregisterCollector(
	ctx context.Context,
	req *connect.Request[collectorv1.UnregisterCollectorRequest],
) (*connect.Response[collectorv1.UnregisterCollectorResponse], error) {
	log := logf.FromContext(ctx)
	if err := s.registry.Unregister(ctx, req.Msg.GetId()); err != nil {
		// Best-effort for consistency with RegisterCollector; TTL eviction cleans up.
		s.countRegistryError("unregister")
		log.Error(err, "Failed to unregister collector", "id", req.Msg.GetId())
	}
	return connect.NewResponse(&collectorv1.UnregisterCollectorResponse{}), nil
}
