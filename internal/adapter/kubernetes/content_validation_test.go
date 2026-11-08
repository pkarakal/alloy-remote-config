// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"crypto/sha256"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/composition"
	"github.com/pkarakal/alloy-remote-config/internal/port"
	"github.com/pkarakal/alloy-remote-config/internal/validate"
)

var _ = Describe("ConfigResolver content validation", func() {
	const namespace = "default"
	const (
		tenant  = "cv-tenant"
		good    = "logging {\n  level = \"info\"\n}\n"
		newer   = "logging {\n  level = \"debug\"\n}\n"
		bad     = "logging {\n  level = \n"
		additiv = "local.file \"cv\" {\n  filename = \"/tmp/x\"\n}\n"
	)
	names := []string{"cv-base", "cv-add"}
	sum := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

	// createPC stores a PipelineConfig whose status is as the reconciler would
	// leave it after the spec moved from lastValid to spec.
	createPC := func(name, spec, lastValid string, conds ...metav1.Condition) {
		pc := &fleetv1alpha1.PipelineConfig{Name: name, Namespace: namespace,
			Spec: fleetv1alpha1.PipelineConfigSpec{Content: spec}}
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		pc.Status.ContentHash = sum(spec)
		pc.Status.Conditions = conds
		if lastValid != "" {
			pc.Status.LastValidContent, pc.Status.LastValidHash = lastValid, sum(lastValid)
		}
		Expect(k8sClient.Status().Update(ctx, pc)).To(Succeed())
	}
	cond := func(t string, s metav1.ConditionStatus) metav1.Condition {
		return metav1.Condition{Type: t, Status: s, Reason: "Test", LastTransitionTime: metav1.Now()}
	}
	bind := func(name, ref string, additive bool) {
		spec := fleetv1alpha1.CollectorGroupBindingSpec{TenantRef: tenant, PipelineConfigRef: ref}
		if additive {
			spec.Mode = fleetv1alpha1.BindingModeAdditive
		}
		Expect(k8sClient.Create(ctx, &fleetv1alpha1.CollectorGroupBinding{Name: name, Namespace: namespace, Spec: spec})).To(Succeed())
	}
	resolve := func(mode validate.Mode) (*port.ResolvedConfig, error) {
		return NewConfigResolver(mgrClient, WithContentValidation(mode)).Resolve(ctx, namespace, map[string]string{"tenant": tenant})
	}

	cleanup := func() {
		for _, n := range names {
			pc := &fleetv1alpha1.PipelineConfig{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, pc); err == nil {
				pc.Finalizers = nil
				_ = k8sClient.Update(ctx, pc)
				_ = k8sClient.Delete(ctx, pc)
			}
			b := &fleetv1alpha1.CollectorGroupBinding{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, b); err == nil {
				b.Finalizers = nil
				_ = k8sClient.Update(ctx, b)
				_ = k8sClient.Delete(ctx, b)
			}
		}
	}
	BeforeEach(cleanup)
	AfterEach(cleanup)

	DescribeTable("a base config after an invalid edit",
		func(mode validate.Mode, wantContent string) {
			createPC("cv-base", bad, good, cond(fleetv1alpha1.ConditionTypeContentValid, metav1.ConditionFalse))
			bind("cv-base", "cv-base", false)
			Eventually(func(g Gomega) {
				got, err := resolve(mode)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got.Content).To(Equal(wantContent))
				g.Expect(got.ContentHash).To(Equal(sum(wantContent)))
			}).Should(Succeed())
		},
		Entry("off serves the spec", validate.ModeOff, bad),
		Entry("warn serves the spec", validate.ModeWarn, bad),
		Entry("enforce serves the last valid content", validate.ModeEnforce, good),
	)

	It("answers Unavailable for content that was never valid in enforce", func() {
		createPC("cv-base", bad, "", cond(fleetv1alpha1.ConditionTypeContentValid, metav1.ConditionFalse))
		bind("cv-base", "cv-base", false)
		Eventually(func(g Gomega) {
			_, err := resolve(validate.ModeEnforce)
			g.Expect(err).To(MatchError(port.ErrConfigNotReady))
		}).Should(Succeed())
		got, err := resolve(validate.ModeWarn)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Content).To(Equal(bad))
	})

	It("serves unvalidated content flagged by seeding in enforce", func() {
		createPC("cv-base", bad, "",
			cond(fleetv1alpha1.ConditionTypeContentValid, metav1.ConditionFalse),
			cond(fleetv1alpha1.ConditionTypeSeededUnvalidated, metav1.ConditionTrue))
		bind("cv-base", "cv-base", false)
		Eventually(func(g Gomega) {
			got, err := resolve(validate.ModeEnforce)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Content).To(Equal(bad))
		}).Should(Succeed())
	})

	It("composes an additive from its last valid content in enforce", func() {
		createPC("cv-base", good, good, cond(fleetv1alpha1.ConditionTypeContentValid, metav1.ConditionTrue))
		createPC("cv-add", bad, additiv, cond(fleetv1alpha1.ConditionTypeContentValid, metav1.ConditionFalse))
		bind("cv-base", "cv-base", false)
		bind("cv-add", "cv-add", true)
		want := composition.Compose(good, sum(good), []composition.Additive{{BindingName: "cv-add", Content: additiv}})
		Eventually(func(g Gomega) {
			got, err := resolve(validate.ModeEnforce)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Content).To(Equal(want.Content))
			g.Expect(got.Additives).To(Equal([]string{"cv-add"}))
		}).Should(Succeed())

		By("the same state in warn drops the additive that does not parse")
		got, err := resolve(validate.ModeWarn)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Content).To(Equal(good))
		Expect(got.Additives).To(BeEmpty())
	})

	It("uses a newer valid edit once the reconciler records it", func() {
		createPC("cv-base", newer, newer, cond(fleetv1alpha1.ConditionTypeContentValid, metav1.ConditionTrue))
		bind("cv-base", "cv-base", false)
		Eventually(func(g Gomega) {
			got, err := resolve(validate.ModeEnforce)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.ContentHash).To(Equal(sum(newer)))
		}).Should(Succeed())
	})
})
