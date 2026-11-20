// Copyright (c) 2026 Pavlos Karakalidis
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1alpha1 "github.com/pkarakal/alloy-remote-config/api/v1alpha1"
	"github.com/pkarakal/alloy-remote-config/internal/composition"
	"github.com/pkarakal/alloy-remote-config/internal/validate"
)

// log is for logging in this package.
var pipelineconfiglog = logf.Log.WithName("pipelineconfig-resource")

// SetupPipelineConfigWebhookWithManager registers the webhook for PipelineConfig in the manager.
func SetupPipelineConfigWebhookWithManager(mgr ctrl.Manager, mode validate.Mode) error {
	return ctrl.NewWebhookManagedBy(mgr, &fleetv1alpha1.PipelineConfig{}).
		WithValidator(&PipelineConfigCustomValidator{Mode: mode}).
		Complete()
}

// The chart overrides failurePolicy through webhook.failurePolicy.
// +kubebuilder:webhook:path=/validate-fleet-pkarakal-com-v1alpha1-pipelineconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=fleet.pkarakal.com,resources=pipelineconfigs,verbs=create;update,versions=v1alpha1,name=vpipelineconfig-v1alpha1.kb.io,admissionReviewVersions=v1

// PipelineConfigCustomValidator checks the Alloy syntax of spec.content. Blank
// content is already refused by the CRD. Deletes are always allowed.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PipelineConfigCustomValidator struct {
	// Mode is the same --content-validation mode the reconciler runs in.
	Mode validate.Mode
}

var contentPath = field.NewPath("spec", "content")

// check parses content per the mode: off allows silently, warn allows with a
// warning, enforce rejects.
func (v *PipelineConfigCustomValidator) check(content string) (admission.Warnings, error) {
	if !v.Mode.Parses() {
		return nil, nil
	}
	perr := composition.CheckConfig(content)
	if perr == nil {
		return nil, nil
	}
	msg := fleetv1alpha1.TruncateError(perr.Error())
	if v.Mode == validate.ModeEnforce {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: fleetv1alpha1.GroupVersion.Group, Kind: "PipelineConfig"}, "",
			field.ErrorList{field.Invalid(contentPath, "(omitted)", "Alloy syntax error: "+msg)})
	}
	return admission.Warnings{fmt.Sprintf("%s: Alloy syntax error: %s; the config is still served (content-validation=warn)",
		contentPath, msg)}, nil
}

// ValidateCreate checks the syntax of the new content.
func (v *PipelineConfigCustomValidator) ValidateCreate(_ context.Context, obj *fleetv1alpha1.PipelineConfig) (admission.Warnings, error) {
	pipelineconfiglog.V(1).Info("Validating PipelineConfig on create", "name", obj.GetName())
	return v.check(obj.Spec.Content)
}

// ValidateUpdate re-checks only when spec.content changed, so status and
// metadata updates of an existing invalid object are never blocked.
func (v *PipelineConfigCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj *fleetv1alpha1.PipelineConfig) (admission.Warnings, error) {
	pipelineconfiglog.V(1).Info("Validating PipelineConfig on update", "name", newObj.GetName())
	if oldObj.Spec.Content == newObj.Spec.Content {
		return nil, nil
	}
	return v.check(newObj.Spec.Content)
}

// ValidateDelete always allows.
func (v *PipelineConfigCustomValidator) ValidateDelete(_ context.Context, _ *fleetv1alpha1.PipelineConfig) (admission.Warnings, error) {
	return nil, nil
}
