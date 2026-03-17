// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	// mgrClient is a cache-backed client with field indexes registered.
	// Use it in tests that call functions relying on MatchingFields lookups.
	mgrClient client.Client
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = fleetv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	// Raw client — used by most tests for direct CRUD without cache lag.
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Manager — provides a cache-backed client with field indexes registered.
	// Used only by tests that need MatchingFields lookups (e.g. enqueue tests).
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	indexer := mgr.GetFieldIndexer()
	Expect(indexer.IndexField(ctx, &fleetv1alpha1.CollectorGroupBinding{}, IndexTenantRef,
		func(obj client.Object) []string {
			b := obj.(*fleetv1alpha1.CollectorGroupBinding)
			if b.Spec.TenantRef == "" {
				return nil
			}
			return []string{b.Spec.TenantRef}
		},
	)).To(Succeed())
	Expect(indexer.IndexField(ctx, &fleetv1alpha1.CollectorGroupBinding{}, IndexCollectorGroupRef,
		func(obj client.Object) []string {
			b := obj.(*fleetv1alpha1.CollectorGroupBinding)
			if b.Spec.CollectorGroupRef == "" {
				return nil
			}
			return []string{b.Spec.CollectorGroupRef}
		},
	)).To(Succeed())
	Expect(indexer.IndexField(ctx, &fleetv1alpha1.CollectorGroupBinding{}, IndexPipelineConfigRef,
		func(obj client.Object) []string {
			b := obj.(*fleetv1alpha1.CollectorGroupBinding)
			return []string{b.Spec.PipelineConfigRef}
		},
	)).To(Succeed())

	mgrClient = mgr.GetClient()

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()

	// Wait for the cache to sync before tests that use mgrClient run.
	Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	Eventually(func() error {
		return testEnv.Stop()
	}, time.Minute, time.Second).Should(Succeed())
})

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
