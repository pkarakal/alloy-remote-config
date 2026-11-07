// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package compat

import (
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
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
				old.expectMatchesGolden(sc, as.Attrs, golden[as.Name])
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

// Blank content is rejected on create, but objects stored before the rule must
// survive validation ratcheting on unrelated updates and must never be served.
var _ = Describe("blank PipelineConfig content", func() {
	DescribeTable("is rejected on create under the current CRDs",
		func(content string) {
			ns := &corev1.Namespace{Name: "compat-blank-rejected"}
			Expect(client.IgnoreAlreadyExists(current.client.Create(ctx, ns))).To(Succeed())
			pc := &fleetv1alpha1.PipelineConfig{
				Namespace: ns.Name, GenerateName: "blank-",
				Spec: fleetv1alpha1.PipelineConfigSpec{Content: content},
			}
			err := current.client.Create(ctx, pc)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		},
		Entry("empty", ""),
		Entry("spaces", "   "),
		Entry("newlines and tabs", "\n\t \n"),
	)

	Context("created under crds-old", Ordered, func() {
		const ns = "compat-blank-preexisting"
		key := client.ObjectKey{Namespace: ns, Name: "default-config"}
		var old *harness

		BeforeAll(func() {
			if updateGoldens() {
				Skip("goldens are being rewritten")
			}
			old = startHarness("crds-old")
			DeferCleanup(func() { old.stop() })

			Expect(old.client.Create(ctx, &corev1.Namespace{Name: ns})).To(Succeed())
			Expect(old.client.Create(ctx, &fleetv1alpha1.PipelineConfig{
				Namespace: ns, Name: key.Name,
				Labels: map[string]string{fleetv1alpha1.LabelDefaultPipelineConfig: "true"},
				Spec:   fleetv1alpha1.PipelineConfigSpec{Content: ""},
			})).To(Succeed())
			old.applyCRDs(filepath.Join("..", "..", "config", "crd", "bases"))
		})

		It("accepts a no-op update through ratcheting", func() {
			live := &fleetv1alpha1.PipelineConfig{}
			Expect(old.client.Get(ctx, key, live)).To(Succeed())
			Expect(old.client.Update(ctx, live)).To(Succeed())
		})

		It("answers Unavailable and never serves a hash", func() {
			Eventually(func(g Gomega) {
				live := &fleetv1alpha1.PipelineConfig{}
				g.Expect(old.mgr.GetClient().Get(ctx, key, live)).To(Succeed())
				g.Expect(live.Status.ObservedGeneration).To(Equal(live.Generation))
				g.Expect(live.Status.ContentHash).To(BeEmpty())
			}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
			_, err := old.serviceFor(ns).GetConfig(ctx, connect.NewRequest(&collectorv1.GetConfigRequest{
				Id: "compat-collector", LocalAttributes: map[string]string{},
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
		})
	})
})
