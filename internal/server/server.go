// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const shutdownTimeout = 5 * time.Second

// ConnectServer wraps an http.Server as a controller-runtime Runnable so it
// can be managed by the operator's manager lifecycle.
type ConnectServer struct {
	srv *http.Server
}

var (
	_ manager.Runnable               = (*ConnectServer)(nil)
	_ manager.LeaderElectionRunnable = (*ConnectServer)(nil)
)

// NewConnectServer creates a ConnectServer that serves the given handler on addr.
func NewConnectServer(handler http.Handler, addr string) *ConnectServer {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)

	return &ConnectServer{
		srv: &http.Server{
			Addr:      addr,
			Handler:   handler,
			Protocols: p,
		},
	}
}

// Start begins serving and blocks until ctx is cancelled, then performs a
// graceful shutdown.
func (s *ConnectServer) Start(ctx context.Context) error {
	log := logf.FromContext(ctx)
	log.Info("Starting Connect-RPC server", "addr", s.srv.Addr)

	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("Shutting down Connect-RPC server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	}
}

// NeedLeaderElection returns false — config reads are safe on all replicas.
func (s *ConnectServer) NeedLeaderElection() bool {
	return false
}
