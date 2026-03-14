package main

import (
	"context"
	"log"
	"net/http"

	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	"github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1/collectorv1connect"
)

type CollectorService struct{}

func (c *CollectorService) GetConfig(context.Context, *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error) {
	return nil, nil
}

func (c *CollectorService) RegisterCollector(context.Context, *connect.Request[collectorv1.RegisterCollectorRequest]) (*connect.Response[collectorv1.RegisterCollectorResponse], error) {
	return nil, nil
}

func (c *CollectorService) UnregisterCollector(context.Context, *connect.Request[collectorv1.UnregisterCollectorRequest]) (*connect.Response[collectorv1.UnregisterCollectorResponse], error) {
	return nil, nil
}

func main() {
	reflector := grpcreflect.NewStaticReflector(
		"collector.v1.CollectorService",
	)

	mux := http.NewServeMux()

	mux.Handle(
		collectorv1connect.NewCollectorServiceHandler(&CollectorService{}),
	)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))

	p := new(http.Protocols)
	p.SetHTTP1(true)
	// For gRPC clients, it's convenient to support HTTP/2 without TLS.
	p.SetUnencryptedHTTP2(true)
	s := &http.Server{
		Addr:      "localhost:8080",
		Handler:   mux,
		Protocols: p,
	}
	if err := s.ListenAndServe(); err != nil {
		log.Fatalf("listen failed: %v", err)
	}
}
