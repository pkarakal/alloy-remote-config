// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/validate"
)

const (
	goodConfig = "logging {\n  level = \"info\"\n}\n"
	badConfig  = "logging {\n  level = \n"
)

func hashOf(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

var _ = Describe("PipelineConfig content validation", func() {
	ctx := context.Background()
	key := types.NamespacedName{Name: "validated", Namespace: "default"}

	// setup builds a reconciler over a fake client holding pc (with its status).
	setup := func(mode validate.Mode, pc *fleetv1alpha1.PipelineConfig) (*PipelineConfigReconciler, client.Client) {
		pc.Name, pc.Namespace = key.Name, key.Namespace
		pc.Generation = 1
		pc.Finalizers = []string{PipelineConfigFinalizer}
		fc := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).
			WithObjects(pc).WithStatusSubresource(pc).
			WithIndex(&fleetv1alpha1.CollectorGroupBinding{}, IndexPipelineConfigRef,
				func(o client.Object) []string {
					return []string{o.(*fleetv1alpha1.CollectorGroupBinding).Spec.PipelineConfigRef}
				}).Build()
		return &PipelineConfigReconciler{Client: fc, Scheme: fc.Scheme(), ContentValidation: mode}, fc
	}
	// reconcile runs twice: a second pass must change nothing, which is what
	// proves a new object is not mistaken for a pre-existing one.
	reconcileTwice := func(r *PipelineConfigReconciler, fc client.Client) *fleetv1alpha1.PipelineConfig {
		for range 2 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		got := &fleetv1alpha1.PipelineConfig{}
		Expect(fc.Get(ctx, key, got)).To(Succeed())
		return got
	}
	edit := func(fc client.Client, content string) {
		pc := &fleetv1alpha1.PipelineConfig{}
		Expect(fc.Get(ctx, key, pc)).To(Succeed())
		pc.Spec.Content = content
		pc.Generation++
		Expect(fc.Update(ctx, pc)).To(Succeed())
	}
	valid := func(pc *fleetv1alpha1.PipelineConfig) *metav1.Condition {
		return meta.FindStatusCondition(pc.Status.Conditions, fleetv1alpha1.ConditionTypeContentValid)
	}

	DescribeTable("a new object",
		func(mode validate.Mode, content string, wantStatus metav1.ConditionStatus, wantReason string, wantLastValid bool) {
			r, fc := setup(mode, &fleetv1alpha1.PipelineConfig{Spec: fleetv1alpha1.PipelineConfigSpec{Content: content}})
			got := reconcileTwice(r, fc)

			Expect(got.Status.ContentHash).To(Equal(hashOf(content)), "contentHash keeps its meaning in every mode")
			Expect(valid(got)).NotTo(BeNil())
			Expect(valid(got).Status).To(Equal(wantStatus))
			Expect(valid(got).Reason).To(Equal(wantReason))
			if wantLastValid {
				Expect(got.Status.LastValidContent).To(Equal(content))
				Expect(got.Status.LastValidHash).To(Equal(hashOf(content)))
			} else {
				Expect(got.Status.LastValidContent).To(BeEmpty())
				Expect(got.Status.LastValidHash).To(BeEmpty())
			}
			Expect(meta.FindStatusCondition(got.Status.Conditions, fleetv1alpha1.ConditionTypeSeededUnvalidated)).To(BeNil(),
				"a new object is never seeded, even on the second pass")
		},
		Entry("off, valid", validate.ModeOff, goodConfig, metav1.ConditionTrue, "ContentValid", false),
		Entry("off, invalid is not parsed", validate.ModeOff, badConfig, metav1.ConditionTrue, "ContentValid", false),
		Entry("warn, valid", validate.ModeWarn, goodConfig, metav1.ConditionTrue, "ContentValid", false),
		Entry("warn, invalid", validate.ModeWarn, badConfig, metav1.ConditionFalse, "SyntaxError", false),
		Entry("enforce, valid", validate.ModeEnforce, goodConfig, metav1.ConditionTrue, "ContentValid", true),
		Entry("enforce, invalid", validate.ModeEnforce, badConfig, metav1.ConditionFalse, "SyntaxError", false),
	)

	It("truncates a long parser message", func() {
		r, fc := setup(validate.ModeWarn, &fleetv1alpha1.PipelineConfig{
			Spec: fleetv1alpha1.PipelineConfigSpec{Content: strings.Repeat("a = = 1\n", 400)},
		})
		got := reconcileTwice(r, fc)
		Expect(len(valid(got).Message)).To(BeNumerically("<=", fleetv1alpha1.MaxLastErrorBytes))
	})

	It("keeps the last valid content after an invalid edit in enforce", func() {
		r, fc := setup(validate.ModeEnforce, &fleetv1alpha1.PipelineConfig{Spec: fleetv1alpha1.PipelineConfigSpec{Content: goodConfig}})
		reconcileTwice(r, fc)

		edit(fc, badConfig)
		got := reconcileTwice(r, fc)
		Expect(got.Status.ContentHash).To(Equal(hashOf(badConfig)))
		Expect(valid(got).Status).To(Equal(metav1.ConditionFalse))
		Expect(got.Status.LastValidContent).To(Equal(goodConfig))
		Expect(got.Status.LastValidHash).To(Equal(hashOf(goodConfig)))
		Expect(got.Status.ValidatedGeneration).To(Equal(got.Generation))

		next := "logging {\n  level = \"debug\"\n}\n"
		edit(fc, next)
		got = reconcileTwice(r, fc)
		Expect(valid(got).Status).To(Equal(metav1.ConditionTrue))
		Expect(got.Status.LastValidContent).To(Equal(next))
	})

	It("keeps the last valid content in warn too, without tracking it", func() {
		r, fc := setup(validate.ModeWarn, &fleetv1alpha1.PipelineConfig{Spec: fleetv1alpha1.PipelineConfigSpec{Content: goodConfig}})
		reconcileTwice(r, fc)
		edit(fc, badConfig)
		got := reconcileTwice(r, fc)
		Expect(got.Status.LastValidContent).To(BeEmpty())
		Expect(got.Status.ContentHash).To(Equal(hashOf(badConfig)))
	})

	Describe("upgrading an object that predates enforce", func() {
		predating := func(content string) *fleetv1alpha1.PipelineConfig {
			return &fleetv1alpha1.PipelineConfig{
				Spec: fleetv1alpha1.PipelineConfigSpec{Content: content},
				Status: fleetv1alpha1.PipelineConfigStatus{
					ContentHash: hashOf(content),
					Conditions: []metav1.Condition{{
						Type: fleetv1alpha1.ConditionTypeContentValid, Status: metav1.ConditionTrue,
						Reason: "ContentValid", LastTransitionTime: metav1.Now(),
					}},
				},
			}
		}

		It("seeds parseable content", func() {
			r, fc := setup(validate.ModeEnforce, predating(goodConfig))
			got := reconcileTwice(r, fc)
			Expect(got.Status.LastValidContent).To(Equal(goodConfig))
			Expect(meta.FindStatusCondition(got.Status.Conditions, fleetv1alpha1.ConditionTypeSeededUnvalidated)).To(BeNil())
		})

		It("keeps serving unparseable content and flags it", func() {
			r, fc := setup(validate.ModeEnforce, predating(badConfig))
			got := reconcileTwice(r, fc)
			Expect(got.Status.LastValidContent).To(BeEmpty())
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, fleetv1alpha1.ConditionTypeSeededUnvalidated)).To(BeTrue())
			content, hash, ok := validate.Served(got, validate.ModeEnforce)
			Expect(ok).To(BeTrue())
			Expect(content).To(Equal(badConfig))
			Expect(hash).To(Equal(hashOf(badConfig)))

			By("a valid edit replaces it and clears the flag")
			edit(fc, goodConfig)
			got = reconcileTwice(r, fc)
			Expect(got.Status.LastValidContent).To(Equal(goodConfig))
			Expect(meta.FindStatusCondition(got.Status.Conditions, fleetv1alpha1.ConditionTypeSeededUnvalidated)).To(BeNil())
		})
	})

	It("copies nothing above the size guard and flags it", func() {
		big := "// " + strings.Repeat("x", fleetv1alpha1.MaxLastValidContentBytes) + "\n" + goodConfig
		r, fc := setup(validate.ModeEnforce, &fleetv1alpha1.PipelineConfig{Spec: fleetv1alpha1.PipelineConfigSpec{Content: big}})
		got := reconcileTwice(r, fc)
		Expect(got.Status.LastValidContent).To(BeEmpty())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, fleetv1alpha1.ConditionTypeContentTooLarge)).To(BeTrue())
		Expect(valid(got).Status).To(Equal(metav1.ConditionTrue))
		content, hash, ok := validate.Served(got, validate.ModeEnforce)
		Expect(ok).To(BeTrue())
		Expect(content).To(Equal(big))
		Expect(hash).To(Equal(hashOf(big)))

		By("shrinking it back resumes last-known-good")
		edit(fc, goodConfig)
		got = reconcileTwice(r, fc)
		Expect(meta.FindStatusCondition(got.Status.Conditions, fleetv1alpha1.ConditionTypeContentTooLarge)).To(BeNil())
		Expect(got.Status.LastValidContent).To(Equal(goodConfig))
	})
})

var _ = DescribeTable("validate.Served",
	func(mode validate.Mode, st fleetv1alpha1.PipelineConfigStatus, wantOK bool, wantContent string) {
		pc := &fleetv1alpha1.PipelineConfig{Spec: fleetv1alpha1.PipelineConfigSpec{Content: "spec"}, Status: st}
		content, _, ok := validate.Served(pc, mode)
		Expect(ok).To(Equal(wantOK))
		Expect(content).To(Equal(wantContent))
	},
	Entry("not reconciled", validate.ModeEnforce, fleetv1alpha1.PipelineConfigStatus{}, false, ""),
	Entry("off serves spec", validate.ModeOff, fleetv1alpha1.PipelineConfigStatus{ContentHash: "h", LastValidContent: "old"}, true, "spec"),
	Entry("warn serves spec", validate.ModeWarn, fleetv1alpha1.PipelineConfigStatus{ContentHash: "h", LastValidContent: "old"}, true, "spec"),
	Entry("enforce serves last valid", validate.ModeEnforce, fleetv1alpha1.PipelineConfigStatus{ContentHash: "h", LastValidContent: "old", LastValidHash: "oh"}, true, "old"),
	Entry("enforce never valid", validate.ModeEnforce, fleetv1alpha1.PipelineConfigStatus{
		ContentHash: "h",
		Conditions:  []metav1.Condition{{Type: fleetv1alpha1.ConditionTypeContentValid, Status: metav1.ConditionFalse}},
	}, false, ""),
	Entry("enforce on a CRD without the fields degrades to warn", validate.ModeEnforce, fleetv1alpha1.PipelineConfigStatus{
		ContentHash: "h",
		Conditions:  []metav1.Condition{{Type: fleetv1alpha1.ConditionTypeContentValid, Status: metav1.ConditionTrue}},
	}, true, "spec"),
)
