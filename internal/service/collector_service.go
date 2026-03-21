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

	"github.com/pkarakal/alloy-remote-config/internal/port"
)

var _ collectorv1connect.CollectorServiceHandler = (*CollectorService)(nil)

// CollectorService implements the Connect-RPC CollectorServiceHandler by
// delegating to storage-agnostic port interfaces.
type CollectorService struct {
	resolver  port.ConfigResolver
	registry  port.CollectorRegistry
	namespace string
}

// NewCollectorService creates a CollectorService wired to the given ports.
func NewCollectorService(
	resolver port.ConfigResolver,
	registry port.CollectorRegistry,
	namespace string,
) *CollectorService {
	return &CollectorService{
		resolver:  resolver,
		registry:  registry,
		namespace: namespace,
	}
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

	resolved, err := s.resolveConfig(ctx, tenant, collectorGroup)
	if err != nil {
		log.Error(err, "Failed to resolve config")
		return nil, err
	}

	if msg.GetHash() == resolved.ContentHash {
		return connect.NewResponse(&collectorv1.GetConfigResponse{
			NotModified: true,
		}), nil
	}

	return connect.NewResponse(&collectorv1.GetConfigResponse{
		Content: resolved.Content,
		Hash:    resolved.ContentHash,
	}), nil
}

// resolveConfig walks the fallback chain: tenant -> collector group -> default.
func (s *CollectorService) resolveConfig(ctx context.Context, tenant, collectorGroup string) (*port.ResolvedConfig, error) {
	if tenant != "" {
		resolved, err := s.resolver.ResolveByTenant(ctx, s.namespace, tenant)
		if err == nil {
			return resolved, nil
		}
		if !errors.Is(err, port.ErrConfigNotFound) {
			return nil, s.mapError(ctx, err)
		}
	}

	if collectorGroup != "" {
		resolved, err := s.resolver.ResolveByCollectorGroup(ctx, s.namespace, collectorGroup)
		if err == nil {
			return resolved, nil
		}
		if !errors.Is(err, port.ErrConfigNotFound) {
			return nil, s.mapError(ctx, err)
		}
	}

	resolved, err := s.resolver.ResolveDefault(ctx, s.namespace)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return resolved, nil
}

// mapError converts domain errors to Connect error codes.
func (s *CollectorService) mapError(ctx context.Context, err error) *connect.Error {
	log := logf.FromContext(ctx)
	switch {
	case errors.Is(err, port.ErrConfigNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, port.ErrConfigNotReady):
		return connect.NewError(connect.CodeUnavailable, err)
	default:
		log.Error(err, "Internal error during config resolution")
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
		log.Error(err, "Failed to register collector", "id", msg.GetId())
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&collectorv1.RegisterCollectorResponse{}), nil
}

func (s *CollectorService) UnregisterCollector(
	ctx context.Context,
	req *connect.Request[collectorv1.UnregisterCollectorRequest],
) (*connect.Response[collectorv1.UnregisterCollectorResponse], error) {
	log := logf.FromContext(ctx)
	if err := s.registry.Unregister(ctx, req.Msg.GetId()); err != nil {
		log.Error(err, "Failed to unregister collector", "id", req.Msg.GetId())
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&collectorv1.UnregisterCollectorResponse{}), nil
}
