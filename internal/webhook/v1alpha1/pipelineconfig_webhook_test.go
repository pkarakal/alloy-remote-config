// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/validate"
)

const (
	goodConfig = "logging {\n  level = \"info\"\n}\n"
	badConfig  = "logging {\n  level = \n"
)

func pipelineConfig(content string) *fleetv1alpha1.PipelineConfig {
	return &fleetv1alpha1.PipelineConfig{
		Name: "wh", Namespace: "default",
		Spec: fleetv1alpha1.PipelineConfigSpec{Content: content},
	}
}

var _ = Describe("PipelineConfig Webhook", func() {
	DescribeTable("create",
		func(mode validate.Mode, content string, wantWarning, wantReject bool) {
			v := &PipelineConfigCustomValidator{Mode: mode}
			warnings, err := v.ValidateCreate(ctx, pipelineConfig(content))
			if wantReject {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
				Expect(err.Error()).To(ContainSubstring("spec.content"))
				Expect(err.Error()).To(ContainSubstring("Alloy syntax error"))
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
			if wantWarning {
				Expect(warnings).To(HaveLen(1))
				Expect(warnings[0]).To(And(
					HavePrefix("spec.content: Alloy syntax error"),
					ContainSubstring("content:3:"),
					ContainSubstring("still served (content-validation=warn)")))
			} else {
				Expect(warnings).To(BeEmpty())
			}
		},
		Entry("off, valid", validate.ModeOff, goodConfig, false, false),
		Entry("off, invalid is allowed silently", validate.ModeOff, badConfig, false, false),
		Entry("warn, valid", validate.ModeWarn, goodConfig, false, false),
		Entry("warn, invalid is allowed with a warning", validate.ModeWarn, badConfig, true, false),
		Entry("enforce, valid", validate.ModeEnforce, goodConfig, false, false),
		Entry("enforce, invalid is rejected", validate.ModeEnforce, badConfig, false, true),
	)

	DescribeTable("update",
		func(mode validate.Mode, oldContent, newContent string, wantWarning, wantReject bool) {
			v := &PipelineConfigCustomValidator{Mode: mode}
			warnings, err := v.ValidateUpdate(ctx, pipelineConfig(oldContent), pipelineConfig(newContent))
			Expect(err != nil).To(Equal(wantReject), "got %v", err)
			if wantWarning {
				Expect(warnings).NotTo(BeEmpty())
			} else {
				Expect(warnings).To(BeEmpty())
			}
		},
		Entry("warn, content made invalid", validate.ModeWarn, goodConfig, badConfig, true, false),
		Entry("enforce, content made invalid", validate.ModeEnforce, goodConfig, badConfig, false, true),
		Entry("enforce, content fixed", validate.ModeEnforce, badConfig, goodConfig, false, false),
		Entry("enforce, unchanged invalid content is not re-checked", validate.ModeEnforce, badConfig, badConfig, false, false),
		Entry("warn, unchanged invalid content is not re-checked", validate.ModeWarn, badConfig, badConfig, false, false),
	)

	It("always allows delete", func() {
		v := &PipelineConfigCustomValidator{Mode: validate.ModeEnforce}
		warnings, err := v.ValidateDelete(ctx, pipelineConfig(badConfig))
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(BeEmpty())
	})

	// The suite serves the webhook in enforce mode, so this goes through the
	// API server and the admission request path.
	Context("through the API server in enforce mode", func() {
		It("rejects invalid content on create and allows valid content", func() {
			bad := pipelineConfig(badConfig)
			bad.Name = "wh-bad"
			err := k8sClient.Create(ctx, bad)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring("Alloy syntax error"))

			ok := pipelineConfig(goodConfig)
			ok.Name = "wh-ok"
			Expect(k8sClient.Create(ctx, ok)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, ok)).To(Succeed()) })

			By("rejecting an invalid edit")
			ok.Spec.Content = badConfig
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, ok))).To(BeTrue())
		})
	})
})
