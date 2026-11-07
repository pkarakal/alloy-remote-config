// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package interceptor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/pkarakal/alloy-remote-config/internal/metrics"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

const (
	reasonMissing   = "missing"
	reasonAmbiguous = "ambiguous"
	reasonMismatch  = "mismatch"

	// tenantAttribute is the attribute UnregisterCollector can be scoped by.
	tenantAttribute = "tenant"
)

// TrustedAttribute binds a local attribute key to the request header a
// trusted proxy sets for it.
type TrustedAttribute struct {
	Key    string
	Header string
}

// ParseTrustedAttribute parses a "<key>=<Header-Name>" flag value.
func ParseTrustedAttribute(spec string) (TrustedAttribute, error) {
	key, header, ok := strings.Cut(spec, "=")
	if !ok {
		return TrustedAttribute{}, fmt.Errorf("trusted attribute %q must be <key>=<Header-Name>", spec)
	}
	if key == "" {
		return TrustedAttribute{}, fmt.Errorf("trusted attribute %q has an empty key", spec)
	}
	if !isHeaderToken(header) {
		return TrustedAttribute{}, fmt.Errorf("trusted attribute %q has an invalid header name %q", spec, header)
	}
	return TrustedAttribute{Key: key, Header: header}, nil
}

// isHeaderToken reports whether s is a non-empty RFC 9110 token.
func isHeaderToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// TrustedAttributeFlag is a repeatable flag.Value collecting TrustedAttributes.
// Set rejects duplicate keys, so flag.Parse reports the error and exits.
type TrustedAttributeFlag []TrustedAttribute

func (f *TrustedAttributeFlag) String() string {
	parts := make([]string, 0, len(*f))
	for _, a := range *f {
		parts = append(parts, a.Key+"="+a.Header)
	}
	return strings.Join(parts, ",")
}

func (f *TrustedAttributeFlag) Set(v string) error {
	a, err := ParseTrustedAttribute(v)
	if err != nil {
		return err
	}
	for _, e := range *f {
		if e.Key == a.Key {
			return fmt.Errorf("trusted attribute key %q given more than once", a.Key)
		}
	}
	*f = append(*f, a)
	return nil
}

// NewTrustedAttributesInterceptor returns an interceptor that takes the
// configured attribute keys from proxy-set request headers instead of the
// client's self-asserted values. Callers install it only when attrs is non-empty.
//
// The effective attributes are written into a copy assigned to the request
// message's LocalAttributes (the message is per request), so the service,
// resolution, registry and status ingestion all see the same map.
// RegisterCollector and UnregisterCollector never fail: Alloy treats a
// Register error as fatal for remotecfg, so rejected calls are skipped and
// answered with OK. m may be nil.
func NewTrustedAttributesInterceptor(attrs []TrustedAttribute, m *metrics.RPCMetrics) connect.Interceptor {
	t := &trustedAttributes{attrs: attrs, metrics: m}
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if req.Spec().IsClient {
				return next(ctx, req)
			}
			switch msg := req.Any().(type) {
			case *collectorv1.GetConfigRequest:
				effective, reason, key := t.overlay(req, msg.GetLocalAttributes())
				if reason != "" {
					t.reject("GetConfig", reason)
					return nil, rejectionError(reason, key)
				}
				msg.LocalAttributes = effective
			case *collectorv1.RegisterCollectorRequest:
				effective, reason, key := t.overlay(req, msg.GetLocalAttributes())
				if reason != "" {
					t.reject("RegisterCollector", reason)
					logf.FromContext(ctx).V(1).Info("Skipped collector registration, trusted attribute check failed",
						"collectorID", msg.GetId(), "attribute", key, "reason", reason)
					return connect.NewResponse(&collectorv1.RegisterCollectorResponse{}), nil
				}
				msg.LocalAttributes = effective
			case *collectorv1.UnregisterCollectorRequest:
				for _, a := range t.attrs {
					if a.Key != tenantAttribute {
						continue
					}
					value, reason := headerValue(req, a.Header)
					if reason != "" {
						t.reject("UnregisterCollector", reason)
						logf.FromContext(ctx).V(1).Info("Skipped collector unregistration, trusted attribute check failed",
							"collectorID", msg.GetId(), "attribute", a.Key, "reason", reason)
						return connect.NewResponse(&collectorv1.UnregisterCollectorResponse{}), nil
					}
					ctx = port.WithUnregisterScope(ctx, port.UnregisterScope{Tenant: value})
				}
			}
			return next(ctx, req)
		}
	})
}

type trustedAttributes struct {
	attrs   []TrustedAttribute
	metrics *metrics.RPCMetrics
}

// overlay returns a copy of client with every trusted key set from its header.
// On failure it returns the reason and the offending key instead.
func (t *trustedAttributes) overlay(req connect.AnyRequest, client map[string]string) (map[string]string, string, string) {
	effective := make(map[string]string, len(client)+len(t.attrs))
	maps.Copy(effective, client)
	for _, a := range t.attrs {
		value, reason := headerValue(req, a.Header)
		if reason != "" {
			return nil, reason, a.Key
		}
		// An empty claim asserts nothing, so it is overwritten rather than rejected.
		if claimed := client[a.Key]; claimed != "" && claimed != value {
			return nil, reasonMismatch, a.Key
		}
		effective[a.Key] = value
	}
	return effective, "", ""
}

// headerValue returns the single non-empty value of a header, or why it has none.
func headerValue(req connect.AnyRequest, name string) (string, string) {
	var value string
	for _, v := range req.Header().Values(name) {
		v = strings.TrimSpace(v)
		switch {
		case v == "":
		case value == "":
			value = v
		case value != v:
			return "", reasonAmbiguous
		}
	}
	if value == "" {
		return "", reasonMissing
	}
	return value, ""
}

func (t *trustedAttributes) reject(procedure, reason string) {
	if t.metrics != nil && t.metrics.TrustedAttrRejections != nil {
		t.metrics.TrustedAttrRejections.WithLabelValues(procedure, reason).Inc()
	}
}

func rejectionError(reason, key string) *connect.Error {
	if reason == reasonMismatch {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("attribute %q does not match the authenticated identity", key))
	}
	return connect.NewError(connect.CodeUnauthenticated,
		errors.New("trusted attribute "+key+" is "+reason+" in the request headers"))
}
