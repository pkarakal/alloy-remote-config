// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package compat

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
)

// Exercises the binding watches on the real manager, which the controller
// unit tests bypass by calling Reconcile directly.
var _ = Describe("derived activeBindings", func() {
	const ns = "compat-counts"

	pcCount := func(name string) func() int {
		return func() int {
			pc := &fleetv1alpha1.PipelineConfig{}
			if err := current.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, pc); err != nil {
				return -1
			}
			return pc.Status.ActiveBindings
		}
	}
	cgCount := func(name string) func() int {
		return func() int {
			cg := &fleetv1alpha1.CollectorGroup{}
			if err := current.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cg); err != nil {
				return -1
			}
			return cg.Status.ActiveBindings
		}
	}

	It("recounts both sides when a binding moves and blocks deletion while referenced", func() {
		Expect(client.IgnoreAlreadyExists(current.client.Create(ctx, &corev1.Namespace{Name: ns}))).To(Succeed())
		for _, name := range []string{"pc-a", "pc-b"} {
			Expect(current.client.Create(ctx, &fleetv1alpha1.PipelineConfig{
				Name: name, Namespace: ns, Spec: fleetv1alpha1.PipelineConfigSpec{Content: "logging {}"},
			})).To(Succeed())
		}
		Expect(current.client.Create(ctx, &fleetv1alpha1.CollectorGroup{Name: "grp", Namespace: ns})).To(Succeed())
		Expect(current.client.Create(ctx, &fleetv1alpha1.CollectorGroupBinding{
			Name: "bind", Namespace: ns,
			Spec: fleetv1alpha1.CollectorGroupBindingSpec{CollectorGroupRef: "grp", PipelineConfigRef: "pc-a"},
		})).To(Succeed())

		Eventually(pcCount("pc-a"), 30*time.Second).Should(Equal(1))
		Eventually(cgCount("grp"), 30*time.Second).Should(Equal(1))
		Expect(pcCount("pc-b")()).To(Equal(0))

		b := &fleetv1alpha1.CollectorGroupBinding{}
		Expect(current.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "bind"}, b)).To(Succeed())
		b.Spec.PipelineConfigRef = "pc-b"
		Expect(current.client.Update(ctx, b)).To(Succeed())

		Eventually(pcCount("pc-a"), 30*time.Second).Should(Equal(0))
		Eventually(pcCount("pc-b"), 30*time.Second).Should(Equal(1))

		By("blocking deletion of the referenced PipelineConfig until the binding is gone")
		Expect(current.client.Delete(ctx, &fleetv1alpha1.PipelineConfig{Name: "pc-b", Namespace: ns})).To(Succeed())
		Consistently(pcCount("pc-b"), 3*time.Second).Should(Equal(1))
		Expect(current.client.Delete(ctx, b)).To(Succeed())
		Eventually(pcCount("pc-b"), 60*time.Second).Should(Equal(-1))
	})
})
