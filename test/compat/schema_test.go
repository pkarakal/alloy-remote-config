// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package compat

import (
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Schema upgrade: objects created under the previous CRDs (crds-old/) must
// keep validating and serving the same goldens after the current CRDs
// (config/crd/bases) are applied over them. Old equals new today; when a CRD
// change lands, crds-old/ keeps the pre-change schema so this proves the
// upgrade path for existing objects.
var _ = Describe("schema upgrade from crds-old", Ordered, func() {
	var old *harness

	BeforeAll(func() {
		if updateGoldens() {
			Skip("goldens are being rewritten")
		}
		Expect(apiextensionsv1.AddToScheme(scheme.Scheme)).To(Succeed())
		old = startHarness("crds-old")
		DeferCleanup(func() { old.stop() })

		for _, sc := range scenarios {
			old.install(sc)
		}
		old.applyCRDs(filepath.Join("..", "..", "config", "crd", "bases"))
	})

	for _, sc := range scenarios {
		It("keeps "+sc.Dir+" valid and serving the same golden", func() {
			for _, fixture := range loadFixtures(filepath.Join("fixtures", sc.Dir)) {
				live := &unstructured.Unstructured{}
				live.SetGroupVersionKind(fixture.GroupVersionKind())
				key := client.ObjectKey{Namespace: sc.namespace(), Name: fixture.GetName()}
				Expect(old.client.Get(ctx, key, live)).To(Succeed())
				// A no-op update goes through the new schema's validation.
				Expect(old.client.Update(ctx, live)).To(Succeed(), "no-op update of %s %s", fixture.GetKind(), key)
			}
			old.settle(sc.namespace())

			golden := readGolden(sc)
			for _, as := range attrSets {
				old.expectMatchesGolden(sc, as.Name, as.Attrs, golden[as.Name])
			}
		})
	}
})

// applyCRDs updates the installed CRD objects in place to the schemas in dir.
func (h *harness) applyCRDs(dir string) {
	GinkgoHelper()
	opts := &envtest.CRDInstallOptions{Paths: []string{dir}}
	Expect(envtest.ReadCRDFiles(opts)).To(Succeed())
	Expect(opts.CRDs).NotTo(BeEmpty())
	for _, want := range opts.CRDs {
		existing := &apiextensionsv1.CustomResourceDefinition{}
		Expect(h.client.Get(ctx, client.ObjectKey{Name: want.Name}, existing)).To(Succeed())
		existing.Spec = want.Spec
		Expect(h.client.Update(ctx, existing)).To(Succeed(), "updating CRD %s", want.Name)

		Eventually(func(g Gomega) {
			g.Expect(h.client.Get(ctx, client.ObjectKey{Name: want.Name}, existing)).To(Succeed())
			established := false
			for _, c := range existing.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					established = true
				}
			}
			g.Expect(established).To(BeTrue())
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
	}
}
