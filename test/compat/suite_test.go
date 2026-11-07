// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

// Package compat pins the config content, hash and error codes the service
// serves today, so later resolver changes can prove existing setups are
// unaffected. See README.md in this directory.
package compat

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/adapter/kubernetes"
	"github.com/pkarakal/alloy-remote-config/internal/adapter/noop"
	"github.com/pkarakal/alloy-remote-config/internal/controller"
	"github.com/pkarakal/alloy-remote-config/internal/service"
)

var update = flag.Bool("update", false, "rewrite golden files from the currently served behavior")

// updateGoldens reports whether goldens should be rewritten instead of compared.
func updateGoldens() bool { return *update || os.Getenv("COMPAT_UPDATE") == "1" }

var (
	ctx    context.Context
	cancel context.CancelFunc
	// current serves from the CRDs in config/crd/bases.
	current *harness
)

func TestCompat(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Compatibility Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.Background())
	Expect(fleetv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	current = startHarness(filepath.Join("..", "..", "config", "crd", "bases"))
})

var _ = AfterSuite(func() {
	current.stop()
	cancel()
})

// harness is an envtest API server plus a manager running all three
// reconcilers, so field indexes, finalizers and content hashes are real.
type harness struct {
	env    *envtest.Environment
	cfg    *rest.Config
	client client.Client // direct, uncached
	mgr    ctrl.Manager
	stopFn context.CancelFunc
}

func startHarness(crdDir string) *harness {
	GinkgoHelper()
	h := &harness{}
	h.env = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstEnvtestBinaryDir(); dir != "" {
		h.env.BinaryAssetsDirectory = dir
	}
	var err error
	h.cfg, err = h.env.Start()
	Expect(err).NotTo(HaveOccurred())

	h.client, err = client.New(h.cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	h.mgr, err = ctrl.NewManager(h.cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Two harnesses run in one process (see schema_test.go) and register
		// controllers under the same names.
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	Expect(err).NotTo(HaveOccurred())

	Expect((&controller.PipelineConfigReconciler{Client: h.mgr.GetClient(), Scheme: h.mgr.GetScheme()}).SetupWithManager(h.mgr)).To(Succeed())
	Expect((&controller.CollectorGroupReconciler{Client: h.mgr.GetClient(), Scheme: h.mgr.GetScheme()}).SetupWithManager(h.mgr)).To(Succeed())
	// SetupWithManager of this reconciler registers the field indexes the resolver needs.
	Expect((&controller.CollectorGroupBindingReconciler{Client: h.mgr.GetClient(), Scheme: h.mgr.GetScheme()}).SetupWithManager(h.mgr)).To(Succeed())

	var mgrCtx context.Context
	mgrCtx, h.stopFn = context.WithCancel(ctx)
	go func() {
		defer GinkgoRecover()
		Expect(h.mgr.Start(mgrCtx)).To(Succeed())
	}()
	Expect(h.mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
	return h
}

func (h *harness) stop() {
	h.stopFn()
	Eventually(h.env.Stop, time.Minute, time.Second).Should(Succeed())
}

// serviceFor returns the real CollectorService wired to the cache-backed resolver.
func (h *harness) serviceFor(namespace string) *service.CollectorService {
	return service.NewCollectorService(
		kubernetes.NewConfigResolver(h.mgr.GetClient()),
		&noop.CollectorRegistry{},
		namespace,
	)
}

// loadFixtures decodes every YAML document in dir/*.yaml.
func loadFixtures(dir string) []*unstructured.Unstructured {
	GinkgoHelper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	Expect(err).NotTo(HaveOccurred())
	Expect(files).NotTo(BeEmpty())
	var objs []*unstructured.Unstructured
	for _, f := range files {
		raw, err := os.ReadFile(f)
		Expect(err).NotTo(HaveOccurred())
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
		for {
			obj := &unstructured.Unstructured{}
			if err := dec.Decode(&obj.Object); err == io.EOF {
				break
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
			if len(obj.Object) == 0 {
				continue
			}
			objs = append(objs, obj)
		}
	}
	return objs
}

// install creates the scenario namespace and fixtures, then waits until the
// reconcilers have settled and the manager cache serves the final state.
func (h *harness) install(sc scenario) {
	GinkgoHelper()
	ns := sc.namespace()
	err := h.client.Create(ctx, &corev1.Namespace{Name: ns})
	if !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	for _, obj := range loadFixtures(filepath.Join("fixtures", sc.Dir)) {
		obj.SetNamespace(ns)
		Expect(h.client.Create(ctx, obj)).To(Succeed(), "creating %s %s", obj.GetKind(), obj.GetName())
	}
	h.settle(ns)
}

// settle waits for every PipelineConfig in ns to be reconciled (hash set unless
// the content is empty, which the controller never hashes) and for the cache
// the resolver reads from to have caught up.
func (h *harness) settle(ns string) {
	GinkgoHelper()
	pcs := &fleetv1alpha1.PipelineConfigList{}
	Expect(h.client.List(ctx, pcs, client.InNamespace(ns))).To(Succeed())
	for _, p := range pcs.Items {
		key := client.ObjectKeyFromObject(&p)
		Eventually(func(g Gomega) {
			live := &fleetv1alpha1.PipelineConfig{}
			g.Expect(h.client.Get(ctx, key, live)).To(Succeed())
			g.Expect(live.Status.ObservedGeneration).To(Equal(live.Generation))
			if live.Spec.Content != "" {
				g.Expect(live.Status.ContentHash).NotTo(BeEmpty())
			}
			cached := &fleetv1alpha1.PipelineConfig{}
			g.Expect(h.mgr.GetClient().Get(ctx, key, cached)).To(Succeed())
			g.Expect(cached.Status.ObservedGeneration).To(Equal(live.Generation))
			g.Expect(cached.Status.ContentHash).To(Equal(live.Status.ContentHash))
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed(), "settling PipelineConfig %s", key)
	}
	bindings := &fleetv1alpha1.CollectorGroupBindingList{}
	Expect(h.client.List(ctx, bindings, client.InNamespace(ns))).To(Succeed())
	for _, b := range bindings.Items {
		key := client.ObjectKeyFromObject(&b)
		Eventually(func(g Gomega) {
			g.Expect(h.mgr.GetClient().Get(ctx, key, &fleetv1alpha1.CollectorGroupBinding{})).To(Succeed())
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed(), "caching CollectorGroupBinding %s", key)
	}
	// The index lookups the resolver uses must see the bindings too.
	for _, b := range bindings.Items {
		indexKey, value := controller.IndexCollectorGroupRef, b.Spec.CollectorGroupRef
		if b.Spec.TenantRef != "" {
			indexKey, value = controller.IndexTenantRef, b.Spec.TenantRef
		}
		Eventually(func(g Gomega) {
			list := &fleetv1alpha1.CollectorGroupBindingList{}
			g.Expect(h.mgr.GetClient().List(ctx, list, client.InNamespace(ns), client.MatchingFields{indexKey: value})).To(Succeed())
			g.Expect(list.Items).NotTo(BeEmpty())
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
	}
}

// firstEnvtestBinaryDir lets the suite run from an IDE without KUBEBUILDER_ASSETS.
func firstEnvtestBinaryDir() string {
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return ""
	}
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}
