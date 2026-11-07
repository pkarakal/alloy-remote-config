// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/controller"
	"github.com/pkarakal/alloy-remote-config/internal/metrics"
	"github.com/pkarakal/alloy-remote-config/internal/port"
)

// patchCountingClient counts successful Status().Patch calls.
type patchCountingClient struct {
	client.Client
	patches atomic.Int64
}

func (c *patchCountingClient) Status() client.SubResourceWriter {
	return &patchCountingWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type patchCountingWriter struct {
	client.SubResourceWriter
	parent *patchCountingClient
}

func (w *patchCountingWriter) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
) error {
	err := w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
	if err == nil {
		w.parent.patches.Add(1)
	}
	return err
}

// testClock is a manually advanced clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var _ = Describe("CollectorRegistry write path", func() {
	const namespace = "default"

	info := func(id, tenant string) port.CollectorInfo {
		return port.CollectorInfo{
			ID:              id,
			LocalAttributes: map[string]string{"tenant": tenant},
		}
	}

	createBinding := func(name, tenantRef string) {
		b := &fleetv1alpha1.CollectorGroupBinding{
			Name: name, Namespace: namespace,
			Spec: fleetv1alpha1.CollectorGroupBindingSpec{
				TenantRef:         tenantRef,
				PipelineConfigRef: "some-pipeline",
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, b)).To(Succeed())
		DeferCleanup(func() {
			got := &fleetv1alpha1.CollectorGroupBinding{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got); err == nil {
				got.Finalizers = nil
				_ = k8sClient.Update(ctx, got)
				_ = k8sClient.Delete(ctx, got)
			}
		})
	}

	getBinding := func(name string) *fleetv1alpha1.CollectorGroupBinding {
		b := &fleetv1alpha1.CollectorGroupBinding{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, b)).To(Succeed())
		return b
	}

	// waitForCache blocks until the manager cache can resolve the binding by tenant.
	waitForCache := func(tenant string) {
		EventuallyWithOffset(1, func() int {
			list := &fleetv1alpha1.CollectorGroupBindingList{}
			_ = mgrClient.List(ctx, list, client.InNamespace(namespace),
				client.MatchingFields{controller.IndexTenantRef: tenant})
			return len(list.Items)
		}).Should(BeNumerically(">", 0))
	}

	Context("distinct tenants on one group binding", func() {
		const bindingName = "write-race-group-binding"

		It("keeps all 20 entries across two registry instances", func() {
			b := &fleetv1alpha1.CollectorGroupBinding{
				Name: bindingName, Namespace: namespace,
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					CollectorGroupRef: "race-group",
					PipelineConfigRef: "some-pipeline",
				},
			}
			Expect(k8sClient.Create(ctx, b)).To(Succeed())
			DeferCleanup(func() {
				got := getBinding(bindingName)
				got.Finalizers = nil
				_ = k8sClient.Update(ctx, got)
				_ = k8sClient.Delete(ctx, got)
			})
			Eventually(func() int {
				list := &fleetv1alpha1.CollectorGroupBindingList{}
				_ = mgrClient.List(ctx, list, client.InNamespace(namespace),
					client.MatchingFields{controller.IndexCollectorGroupRef: "race-group"})
				return len(list.Items)
			}).Should(BeNumerically(">", 0))

			regs := []*CollectorRegistry{
				NewKubernetesCollectorRegistry(mgrClient, k8sClient, namespace),
				NewKubernetesCollectorRegistry(mgrClient, k8sClient, namespace),
			}
			var wg sync.WaitGroup
			errs := make(chan error, 20)
			for i := range 20 {
				wg.Go(func() {
					errs <- regs[i%2].Register(ctx, port.CollectorInfo{
						ID: fmt.Sprintf("c-%d", i),
						LocalAttributes: map[string]string{
							"tenant":          fmt.Sprintf("tenant-%d", i),
							"collector_group": "race-group",
						},
					})
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(getBinding(bindingName).Status.RegisteredTenants).To(HaveLen(20))
		})
	})

	Context("heartbeat throttling", func() {
		const bindingName = "write-throttle-binding"

		newThrottled := func(ttl time.Duration, clock *testClock) (*CollectorRegistry, *patchCountingClient) {
			cc := &patchCountingClient{Client: mgrClient}
			return NewKubernetesCollectorRegistry(cc, k8sClient, namespace,
				WithCollectorTTL(ttl), WithClock(clock.Now)), cc
		}

		It("writes once per TTL/4 for heartbeats and always for Register", func() {
			createBinding(bindingName, "throttle")
			waitForCache("throttle")
			clock := &testClock{t: time.Now().Truncate(time.Second)}
			reg, cc := newThrottled(4*time.Minute, clock)

			Expect(reg.Heartbeat(ctx, info("c1", "throttle"))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(1)))

			clock.Advance(30 * time.Second)
			Expect(reg.Heartbeat(ctx, info("c1", "throttle"))).To(Succeed())
			Expect(reg.Heartbeat(ctx, info("c2", "throttle"))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(1)))

			clock.Advance(31 * time.Second)
			Expect(reg.Heartbeat(ctx, info("c1", "throttle"))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(2)))

			Expect(reg.Register(ctx, info("c1", "throttle"))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(3)))
		})

		It("uses a one minute interval when the TTL is 0", func() {
			createBinding(bindingName, "throttle")
			waitForCache("throttle")
			clock := &testClock{t: time.Now().Truncate(time.Second)}
			reg, cc := newThrottled(0, clock)

			Expect(reg.Heartbeat(ctx, info("c1", "throttle"))).To(Succeed())
			clock.Advance(59 * time.Second)
			Expect(reg.Heartbeat(ctx, info("c1", "throttle"))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(1)))

			clock.Advance(2 * time.Second)
			Expect(reg.Heartbeat(ctx, info("c1", "throttle"))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(2)))
		})

		It("cuts 60 polls from 10 collectors of one tenant to 1 write", func() {
			createBinding(bindingName, "throttle")
			waitForCache("throttle")
			clock := &testClock{t: time.Now().Truncate(time.Second)}
			reg, cc := newThrottled(5*time.Minute, clock)

			polls := 0
			for range 6 {
				for c := range 10 {
					Expect(reg.Heartbeat(ctx, info(fmt.Sprintf("c%d", c), "throttle"))).To(Succeed())
					polls++
				}
				clock.Advance(10 * time.Second)
			}
			Expect(polls).To(Equal(60))
			Expect(cc.patches.Load()).To(Equal(int64(1)))
		})
	})

	Context("apply status", func() {
		const bindingName = "write-status-binding"

		withStatus := func(st port.ConfigStatus, msg string) port.CollectorInfo {
			i := info("c1", "status")
			i.ConfigStatus, i.ConfigError = st, msg
			return i
		}

		setup := func() (*CollectorRegistry, *patchCountingClient, *testClock) {
			createBinding(bindingName, "status")
			waitForCache("status")
			clock := &testClock{t: time.Now().Truncate(time.Second)}
			cc := &patchCountingClient{Client: mgrClient}
			return NewKubernetesCollectorRegistry(cc, k8sClient, namespace,
				WithCollectorTTL(4*time.Minute), WithClock(clock.Now)), cc, clock
		}

		It("writes a status change inside the throttle window but not a repeat", func() {
			reg, cc, clock := setup()
			Expect(reg.Heartbeat(ctx, withStatus(port.ConfigStatusApplied, ""))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(1)))

			clock.Advance(5 * time.Second)
			Expect(reg.Heartbeat(ctx, withStatus(port.ConfigStatusApplied, ""))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(1)))

			Expect(reg.Heartbeat(ctx, withStatus(port.ConfigStatusFailed, "boom"))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(2)))
			e := getBinding(bindingName).Status.RegisteredTenants[0]
			Expect(e.ConfigStatus).To(Equal(fleetv1alpha1.ConfigStatusFailed))
			Expect(e.LastError).To(Equal("boom"))
			Expect(e.StatusChangedAt).NotTo(BeNil())

			Expect(reg.Heartbeat(ctx, withStatus(port.ConfigStatusFailed, "boom"))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(2)))

			Expect(reg.Heartbeat(ctx, withStatus(port.ConfigStatusFailed, "different"))).To(Succeed())
			Expect(cc.patches.Load()).To(Equal(int64(3)))
		})

		It("truncates lastError at 1024 bytes", func() {
			reg, _, _ := setup()
			Expect(reg.Heartbeat(ctx, withStatus(port.ConfigStatusFailed, strings.Repeat("x", 3000)))).To(Succeed())
			Expect(getBinding(bindingName).Status.RegisteredTenants[0].LastError).To(HaveLen(1024))
		})

		It("leaves the stored status untouched on Register", func() {
			reg, _, _ := setup()
			Expect(reg.Heartbeat(ctx, withStatus(port.ConfigStatusFailed, "boom"))).To(Succeed())
			before := getBinding(bindingName).Status.RegisteredTenants[0]

			Expect(reg.Register(ctx, info("c2", "status"))).To(Succeed())
			e := getBinding(bindingName).Status.RegisteredTenants[0]
			Expect(e.CollectorID).To(Equal("c2"))
			Expect(e.ConfigStatus).To(Equal(fleetv1alpha1.ConfigStatusFailed))
			Expect(e.LastError).To(Equal("boom"))
			Expect(e.StatusChangedAt.Time).To(Equal(before.StatusChangedAt.Time))
		})
	})

	Context("registeredTenants cap", func() {
		const bindingName = "write-cap-binding"

		It("rejects new tenants at the cap, still updates existing ones, and counts truncation", func() {
			b := &fleetv1alpha1.CollectorGroupBinding{
				Name: bindingName, Namespace: namespace,
				Spec: fleetv1alpha1.CollectorGroupBindingSpec{
					CollectorGroupRef: "cap-group",
					PipelineConfigRef: "some-pipeline",
				},
			}
			Expect(k8sClient.Create(ctx, b)).To(Succeed())
			DeferCleanup(func() {
				got := getBinding(bindingName)
				got.Finalizers = nil
				_ = k8sClient.Update(ctx, got)
				_ = k8sClient.Delete(ctx, got)
			})
			old := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
			for i := range maxRegisteredTenants {
				b.Status.RegisteredTenants = append(b.Status.RegisteredTenants, fleetv1alpha1.RegisteredTenant{
					ID: fmt.Sprintf("t-%d", i), CollectorID: "seed", LastSeenAt: old,
				})
			}
			Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
			Eventually(func() int {
				list := &fleetv1alpha1.CollectorGroupBindingList{}
				_ = mgrClient.List(ctx, list, client.InNamespace(namespace),
					client.MatchingFields{controller.IndexCollectorGroupRef: "cap-group"})
				return len(list.Items)
			}).Should(BeNumerically(">", 0))

			m := metrics.NewRPCMetrics()
			reg := NewKubernetesCollectorRegistry(mgrClient, k8sClient, namespace, WithMetrics(m))
			withGroup := func(id, tenant string) port.CollectorInfo {
				return port.CollectorInfo{
					ID:              id,
					LocalAttributes: map[string]string{"tenant": tenant, "collector_group": "cap-group"},
				}
			}

			Expect(reg.Register(ctx, withGroup("new", "brand-new"))).To(Succeed())
			Expect(getBinding(bindingName).Status.RegisteredTenants).To(HaveLen(maxRegisteredTenants))
			Expect(testutil.ToFloat64(m.RegistryTruncated)).To(Equal(1.0))

			Expect(reg.Register(ctx, withGroup("updated", "t-5"))).To(Succeed())
			after := getBinding(bindingName)
			Expect(after.Status.RegisteredTenants).To(HaveLen(maxRegisteredTenants))
			Expect(after.Status.RegisteredTenants[5].CollectorID).To(Equal("updated"))
			Expect(testutil.ToFloat64(m.RegistryTruncated)).To(Equal(1.0))
		})
	})
})
