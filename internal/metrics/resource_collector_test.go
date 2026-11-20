// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package metrics

import (
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

func TestMetrics(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Metrics Suite")
}

var _ = Describe("ResourceCollector tenants by status", func() {
	It("sums statuses across bindings and counts unreported entries as unknown", func() {
		scheme := runtime.NewScheme()
		Expect(fleetv1alpha1.AddToScheme(scheme)).To(Succeed())
		binding := func(name string, ts ...fleetv1alpha1.RegisteredTenant) *fleetv1alpha1.CollectorGroupBinding {
			b := &fleetv1alpha1.CollectorGroupBinding{}
			b.Name, b.Namespace = name, "ns"
			b.Status.RegisteredTenants = ts
			return b
		}
		tenant := func(s fleetv1alpha1.ConfigStatus) fleetv1alpha1.RegisteredTenant {
			return fleetv1alpha1.RegisteredTenant{ID: "t", ConfigStatus: s}
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			binding("one", tenant(fleetv1alpha1.ConfigStatusApplied), tenant(fleetv1alpha1.ConfigStatusFailed)),
			binding("two", tenant(fleetv1alpha1.ConfigStatusApplied), tenant(""), tenant(fleetv1alpha1.ConfigStatusApplying)),
		).Build()

		rc := NewResourceCollector(c, "ns")
		const want = `
# HELP alloy_remote_config_registered_tenants_by_status Current number of registered tenant entries by last reported apply status. Every replica reports the same CRD-derived value; aggregate with max, not sum.
# TYPE alloy_remote_config_registered_tenants_by_status gauge
alloy_remote_config_registered_tenants_by_status{namespace="ns",status="applied"} 2
alloy_remote_config_registered_tenants_by_status{namespace="ns",status="applying"} 1
alloy_remote_config_registered_tenants_by_status{namespace="ns",status="failed"} 1
alloy_remote_config_registered_tenants_by_status{namespace="ns",status="unknown"} 1
`
		Expect(testutil.CollectAndCompare(rc, strings.NewReader(want),
			"alloy_remote_config_registered_tenants_by_status")).To(Succeed())
	})
})

var _ = Describe("ResourceCollector binding expiry", func() {
	It("reports seconds until expiresAt only for bindings that set one", func() {
		scheme := runtime.NewScheme()
		Expect(fleetv1alpha1.AddToScheme(scheme)).To(Succeed())
		now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		withExpiry := func(name string, at time.Time) *fleetv1alpha1.CollectorGroupBinding {
			b := &fleetv1alpha1.CollectorGroupBinding{}
			b.Name, b.Namespace = name, "ns"
			t := metav1.NewTime(at)
			b.Spec.ExpiresAt = &t
			return b
		}
		plain := &fleetv1alpha1.CollectorGroupBinding{}
		plain.Name, plain.Namespace = "plain", "ns"
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			plain, withExpiry("soon", now.Add(90*time.Second)), withExpiry("gone", now.Add(-time.Minute)),
		).Build()

		rc := NewResourceCollector(c, "ns")
		rc.now = func() time.Time { return now }
		const want = `
# HELP alloy_remote_config_binding_expires_in_seconds Seconds until spec.expiresAt for bindings that set one; negative once expired. Bounded by the number of bindings with an expiry.
# TYPE alloy_remote_config_binding_expires_in_seconds gauge
alloy_remote_config_binding_expires_in_seconds{binding="gone",namespace="ns"} -60
alloy_remote_config_binding_expires_in_seconds{binding="soon",namespace="ns"} 90
`
		Expect(testutil.CollectAndCompare(rc, strings.NewReader(want),
			"alloy_remote_config_binding_expires_in_seconds")).To(Succeed())
	})
})
