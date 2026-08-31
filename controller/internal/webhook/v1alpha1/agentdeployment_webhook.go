/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apivalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/internal/credentialadmission"
)

var (
	allowedProviderOverrideRootKeys = map[string]struct{}{
		"workload":  {},
		"container": {},
	}
	allowedWorkloadOverrideKeys = map[string]struct{}{
		"podSecurityContext": {},
		"securityContext":    {},
	}
	allowedPodSecurityContextKeys = map[string]struct{}{
		"runAsUser":           {},
		"runAsGroup":          {},
		"runAsNonRoot":        {},
		"fsGroup":             {},
		"supplementalGroups":  {},
		"fsGroupChangePolicy": {},
		"seccompProfile":      {},
	}
	allowedContainerSecurityContextKeys = map[string]struct{}{
		"runAsUser":                {},
		"runAsGroup":               {},
		"runAsNonRoot":             {},
		"allowPrivilegeEscalation": {},
		"readOnlyRootFilesystem":   {},
		"capabilities":             {},
		"seccompProfile":           {},
	}
	allowedCapabilitiesKeys = map[string]struct{}{
		"drop": {},
	}
	allowedSeccompProfileKeys = map[string]struct{}{
		"type":             {},
		"localhostProfile": {},
	}
)

// SetupAgentDeploymentWebhookWithManager registers the credential-attesting
// mutating webhook and the validating webhook for AgentDeployment.
func SetupAgentDeploymentWebhookWithManager(mgr ctrl.Manager, attestor *credentialadmission.Attestor, recordNamespace string) error {
	secretAccess := &sarReviewer{client: mgr.GetClient()}
	records, err := credentialadmission.NewRecordStore(attestor, mgr.GetAPIReader(), mgr.GetClient(), recordNamespace)
	if err != nil {
		return err
	}
	return ctrl.NewWebhookManagedBy(mgr, &airunwayv1alpha1.AgentDeployment{}).
		WithDefaulter(&AgentDeploymentCustomDefaulter{
			SecretAccess: secretAccess,
			Attestor:     attestor,
		}).
		WithValidator(&AgentDeploymentCustomValidator{
			SecretAccess:      secretAccess,
			Attestor:          attestor,
			CredentialRecords: records,
		}).
		Complete()
}

// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch;delete
// +kubebuilder:webhook:path=/mutate-airunway-ai-v1alpha1-agentdeployment,mutating=true,failurePolicy=fail,sideEffects=None,groups=airunway.ai,resources=agentdeployments,verbs=create;update,versions=v1alpha1,name=magentdeployment-v1alpha1.kb.io,admissionReviewVersions=v1,reinvocationPolicy=IfNeeded
// +kubebuilder:webhook:path=/validate-airunway-ai-v1alpha1-agentdeployment,mutating=false,failurePolicy=fail,sideEffects=NoneOnDryRun,groups=airunway.ai,resources=agentdeployments,verbs=create;update,versions=v1alpha1,name=vagentdeployment-v1alpha1.kb.io,admissionReviewVersions=v1

// AgentDeploymentCustomDefaulter stamps a durable proof only after the
// requesting user is authorized to read the referenced Secret. Reconciliation
// verifies this proof before it publishes the Secret reference to a provider.
type AgentDeploymentCustomDefaulter struct {
	SecretAccess SecretAccessReviewer
	Attestor     *credentialadmission.Attestor
}

// Default implements webhook.CustomDefaulter.
func (d *AgentDeploymentCustomDefaulter) Default(ctx context.Context, obj *airunwayv1alpha1.AgentDeployment) error {
	if credentialsRefOf(obj) == nil {
		d.Attestor.Remove(obj)
		return nil
	}

	validator := &AgentDeploymentCustomValidator{SecretAccess: d.SecretAccess}
	if allErrs := validator.validateCredentialAccess(ctx, obj); len(allErrs) > 0 {
		return allErrs.ToAggregate()
	}
	if d.Attestor == nil {
		return fmt.Errorf("credential admission attestor is not configured")
	}
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return fmt.Errorf("cannot attest credential admission: %w", err)
	}
	if req.Operation == admissionv1.Create {
		// The API server assigns UID and generation only after mutating
		// admission. Never preserve a caller-supplied/replayed CREATE proof;
		// validating admission persists a UID-bound bridge record instead.
		d.Attestor.Remove(obj)
		return nil
	}
	if req.Operation != admissionv1.Update {
		return fmt.Errorf("credential admission attestation does not support operation %q", req.Operation)
	}
	if req.DryRun != nil && *req.DryRun {
		// A dry-run response is visible to the requester. Never turn it into a
		// production-signature oracle that can be replayed after admission is
		// temporarily absent; validation still performs the Secret SAR below.
		d.Attestor.Remove(obj)
		return nil
	}

	var oldObj airunwayv1alpha1.AgentDeployment
	if err := json.Unmarshal(req.OldObject.Raw, &oldObj); err != nil {
		return fmt.Errorf("decode old AgentDeployment for credential admission attestation: %w", err)
	}
	expectedGeneration := oldObj.Generation
	if !apiequality.Semantic.DeepEqual(oldObj.Spec, obj.Spec) {
		expectedGeneration++
	}
	return d.Attestor.Stamp(ctx, obj, oldObj.UID, expectedGeneration)
}

// AgentDeploymentCustomValidator validates AgentDeployment resources.
type AgentDeploymentCustomValidator struct {
	// SecretAccess authorizes the requesting user against a referenced Secret,
	// so an AgentDeployment cannot borrow the controller's privilege to read
	// one the author cannot. Nil disables the check, which is only appropriate
	// in unit tests; the manager always wires it.
	SecretAccess SecretAccessReviewer

	// Attestor verifies UPDATE proofs after all mutating admission has run.
	// CredentialRecords persists the CREATE-only UID bridge because validating
	// admission sees server-assigned identity but cannot patch the object.
	Attestor          *credentialadmission.Attestor
	CredentialRecords CredentialAdmissionRecorder
}

type CredentialAdmissionRecorder interface {
	PersistCreate(context.Context, *airunwayv1alpha1.AgentDeployment) error
}

// agentDeploymentMaxNameLength caps AgentDeployment names so every derived
// workload label value stays within Kubernetes' 63-character label-value limit.
const agentDeploymentMaxNameLength = 63

// ValidateCreate validates AgentDeployment on create.
func (v *AgentDeploymentCustomValidator) ValidateCreate(ctx context.Context, obj *airunwayv1alpha1.AgentDeployment) (admission.Warnings, error) {
	allErrs := validateAgentProviderOverrides(obj.Spec.Provider, field.NewPath("spec", "provider", "overrides"))
	allErrs = append(allErrs, validateAgentDeploymentName(obj)...)
	allErrs = append(allErrs, validateExternalAPIBaseURL(obj)...)
	allErrs = append(allErrs, v.validateCredentialAccess(ctx, obj)...)
	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	if err := v.recordCredentialCreate(ctx, obj); err != nil {
		return nil, err
	}
	return nil, nil
}

// validateAgentDeploymentName rejects names that cannot be reused for the
// resources the providers render from them.
//
// The binding constraint is NOT the 253-character object-name limit that the
// CRD itself allows. The container backend fronts each agent with a Service,
// and Service names are validated as RFC 1035 DNS *labels*: at most 63
// characters, lower-case alphanumeric or '-', and they must start with a
// letter. A name like "my.agent" or "7agent" is a perfectly legal custom
// resource name, so it is admitted, and then every reconcile fails when the
// Service apply is rejected — leaving pods running with no Service, no
// published address, and a permanent Failed status.
//
// Validating here means the user is told at kubectl-apply time, in terms of the
// field they control, instead of discovering it in a controller error loop.
func validateAgentDeploymentName(obj *airunwayv1alpha1.AgentDeployment) field.ErrorList {
	if len(obj.Name) > agentDeploymentMaxNameLength {
		return field.ErrorList{field.Invalid(
			field.NewPath("metadata", "name"),
			obj.Name,
			fmt.Sprintf("name must be at most %d characters so derived workload labels stay within Kubernetes' 63-character label-value limit", agentDeploymentMaxNameLength),
		)}
	}
	if errs := apivalidation.IsDNS1035Label(obj.Name); len(errs) > 0 {
		return field.ErrorList{field.Invalid(
			field.NewPath("metadata", "name"),
			obj.Name,
			fmt.Sprintf("name must be a valid DNS-1035 label so the rendered Service can reuse it (%s)", strings.Join(errs, "; ")),
		)}
	}
	return nil
}

func validateExternalAPIBaseURL(obj *airunwayv1alpha1.AgentDeployment) field.ErrorList {
	if obj.Spec.Model.ExternalAPI == nil {
		return nil
	}

	baseURL := obj.Spec.Model.ExternalAPI.BaseURL
	parsed, err := url.Parse(baseURL)
	if err == nil &&
		baseURL == strings.TrimSpace(baseURL) &&
		parsed.IsAbs() &&
		parsed.Host != "" &&
		parsed.Hostname() != "" &&
		(strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) {
		return nil
	}

	return field.ErrorList{field.Invalid(
		field.NewPath("spec", "model", "externalAPI", "baseURL"),
		baseURL,
		"must be an absolute http or https URL with a host",
	)}
}

// ValidateUpdate validates AgentDeployment on update.
func (v *AgentDeploymentCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *airunwayv1alpha1.AgentDeployment) (admission.Warnings, error) {
	return v.validateUpdate(ctx, oldObj, newObj)
}

func (v *AgentDeploymentCustomValidator) validateUpdate(ctx context.Context, oldObj, newObj *airunwayv1alpha1.AgentDeployment) (admission.Warnings, error) {
	allErrs := validateAgentProviderOverrides(newObj.Spec.Provider, field.NewPath("spec", "provider", "overrides"))
	allErrs = append(allErrs, validateExternalAPIBaseURL(newObj)...)
	allErrs = append(allErrs, v.validateCredentialAccess(ctx, newObj)...)
	if oldObj != nil && oldObj.Spec.Framework.Name != newObj.Spec.Framework.Name {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "framework", "name"),
			"framework selection is immutable; create a new AgentDeployment to switch frameworks",
		))
	}
	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	if credentialsRefOf(newObj) != nil {
		req, err := admission.RequestFromContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("cannot verify credential admission attestation: %w", err)
		}
		if req.DryRun != nil && *req.DryRun {
			return nil, nil
		}
		if v.Attestor == nil {
			return nil, fmt.Errorf("credential admission attestor is not configured")
		}
		if err := v.Attestor.Verify(ctx, newObj); err != nil {
			return nil, fmt.Errorf("credential-bearing AgentDeployment update has no valid admission proof: %w", err)
		}
	}
	return nil, nil
}

func (v *AgentDeploymentCustomValidator) recordCredentialCreate(ctx context.Context, obj *airunwayv1alpha1.AgentDeployment) error {
	if credentialsRefOf(obj) == nil {
		return nil
	}
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return fmt.Errorf("cannot persist credential admission record: %w", err)
	}
	if req.DryRun != nil && *req.DryRun {
		return nil
	}
	if v.CredentialRecords == nil {
		return fmt.Errorf("credential admission CREATE record store is not configured")
	}
	return v.CredentialRecords.PersistCreate(ctx, obj)
}

// ValidateDelete performs no validation on delete.
func (v *AgentDeploymentCustomValidator) ValidateDelete(_ context.Context, _ *airunwayv1alpha1.AgentDeployment) (admission.Warnings, error) {
	return nil, nil
}

func validateAgentProviderOverrides(provider *airunwayv1alpha1.AgentProviderSpec, overridesPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if provider == nil || provider.Overrides == nil || len(provider.Overrides.Raw) == 0 {
		return allErrs
	}

	var rawValue interface{}
	decoder := json.NewDecoder(bytes.NewReader(provider.Overrides.Raw))
	decoder.UseNumber()
	if err := decoder.Decode(&rawValue); err != nil {
		allErrs = append(allErrs, field.Invalid(
			overridesPath,
			fmt.Sprintf("<redacted %d bytes>", len(provider.Overrides.Raw)),
			"overrides must be valid JSON",
		))
		return allErrs
	}
	// Decoder.Decode accepts another JSON value after the first. RawExtension
	// must contain exactly one value, matching json.Unmarshal's behavior while
	// retaining json.Number for exact integer validation.
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		allErrs = append(allErrs, field.Invalid(
			overridesPath,
			fmt.Sprintf("<redacted %d bytes>", len(provider.Overrides.Raw)),
			"overrides must contain exactly one JSON value",
		))
		return allErrs
	}

	root, ok := rawValue.(map[string]interface{})
	if !ok {
		allErrs = append(allErrs, field.Invalid(
			overridesPath,
			fmt.Sprintf("<redacted %d bytes>", len(provider.Overrides.Raw)),
			"overrides must be a JSON object",
		))
		return allErrs
	}

	for key, value := range root {
		if _, allowed := allowedProviderOverrideRootKeys[key]; !allowed {
			allErrs = append(allErrs, field.Forbidden(
				overridesPath.Child(key),
				"only workload/container override sections are supported",
			))
			continue
		}
		section, ok := value.(map[string]interface{})
		if !ok {
			allErrs = append(allErrs, field.Invalid(
				overridesPath.Child(key),
				value,
				"override section must be a JSON object",
			))
			continue
		}
		allErrs = append(allErrs, validateWorkloadSecurityOverrides(section, overridesPath.Child(key))...)
	}

	return allErrs
}

func validateWorkloadSecurityOverrides(section map[string]interface{}, sectionPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for key, value := range section {
		if _, allowed := allowedWorkloadOverrideKeys[key]; !allowed {
			allErrs = append(allErrs, field.Forbidden(
				sectionPath.Child(key),
				"only podSecurityContext and securityContext overrides are allowed",
			))
			continue
		}
		obj, ok := value.(map[string]interface{})
		if !ok {
			allErrs = append(allErrs, field.Invalid(
				sectionPath.Child(key),
				value,
				"security override must be a JSON object",
			))
			continue
		}

		switch key {
		case "podSecurityContext":
			allErrs = append(allErrs, validateAllowedObjectKeys(obj, sectionPath.Child(key), allowedPodSecurityContextKeys,
				"unsupported podSecurityContext key")...)
			allErrs = append(allErrs, validatePodSecurityContextValues(obj, sectionPath.Child(key))...)
			if seccompVal, found := obj["seccompProfile"]; found {
				allErrs = append(allErrs, validateSeccompProfile(seccompVal, sectionPath.Child(key, "seccompProfile"))...)
			}
		case "securityContext":
			allErrs = append(allErrs, validateAllowedObjectKeys(obj, sectionPath.Child(key), allowedContainerSecurityContextKeys,
				"unsupported securityContext key")...)
			allErrs = append(allErrs, validateContainerSecurityContextValues(obj, sectionPath.Child(key))...)
			if capsVal, found := obj["capabilities"]; found {
				allErrs = append(allErrs, validateCapabilities(capsVal, sectionPath.Child(key, "capabilities"))...)
			}
			if seccompVal, found := obj["seccompProfile"]; found {
				allErrs = append(allErrs, validateSeccompProfile(seccompVal, sectionPath.Child(key, "seccompProfile"))...)
			}
		}
	}
	return allErrs
}

func validateAllowedObjectKeys(m map[string]interface{}, path *field.Path, allowed map[string]struct{}, detailPrefix string) field.ErrorList {
	var allErrs field.ErrorList
	for key := range m {
		if _, ok := allowed[key]; !ok {
			allErrs = append(allErrs, field.Forbidden(
				path.Child(key),
				fmt.Sprintf("%s %q", detailPrefix, key),
			))
		}
	}
	return allErrs
}

func validateCapabilities(value interface{}, path *field.Path) field.ErrorList {
	obj, ok := value.(map[string]interface{})
	if !ok {
		return field.ErrorList{field.Invalid(path, value, "capabilities override must be a JSON object")}
	}
	allErrs := validateAllowedObjectKeys(obj, path, allowedCapabilitiesKeys, "unsupported capabilities key")
	dropVal, hasDrop := obj["drop"]
	if !hasDrop {
		return allErrs
	}
	dropList, ok := dropVal.([]interface{})
	if !ok {
		allErrs = append(allErrs, field.Invalid(path.Child("drop"), dropVal, "drop must be an array of capability names"))
		return allErrs
	}
	if len(dropList) == 0 {
		allErrs = append(allErrs, field.Invalid(path.Child("drop"), dropVal, "drop must include \"ALL\""))
		return allErrs
	}
	hasAll := false
	for i, item := range dropList {
		capName, ok := item.(string)
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("drop").Index(i), item, "capability name must be a string"))
			continue
		}
		if capName == "ALL" {
			hasAll = true
		}
	}
	if !hasAll {
		allErrs = append(allErrs, field.Invalid(path.Child("drop"), dropVal, "drop must include \"ALL\""))
	}
	return allErrs
}

func validateSeccompProfile(value interface{}, path *field.Path) field.ErrorList {
	obj, ok := value.(map[string]interface{})
	if !ok {
		return field.ErrorList{field.Invalid(path, value, "seccompProfile override must be a JSON object")}
	}
	allErrs := validateAllowedObjectKeys(obj, path, allowedSeccompProfileKeys, "unsupported seccompProfile key")
	typeVal, found := obj["type"]
	if !found {
		allErrs = append(allErrs, field.Required(path.Child("type"), "seccompProfile.type is required"))
		return allErrs
	}
	typeName, ok := typeVal.(string)
	if !ok || typeName == "" {
		allErrs = append(allErrs, field.Invalid(path.Child("type"), typeVal, "seccompProfile.type must be a non-empty string"))
		return allErrs
	}

	localhostProfileVal, hasLocalhostProfile := obj["localhostProfile"]
	switch typeName {
	case "RuntimeDefault":
		if hasLocalhostProfile {
			allErrs = append(allErrs, field.Forbidden(path.Child("localhostProfile"), "localhostProfile is only valid when seccompProfile.type is Localhost"))
		}
	case "Localhost":
		if !hasLocalhostProfile {
			allErrs = append(allErrs, field.Required(path.Child("localhostProfile"), "localhostProfile is required when seccompProfile.type is Localhost"))
			return allErrs
		}
		profile, ok := localhostProfileVal.(string)
		if !ok || profile == "" {
			allErrs = append(allErrs, field.Invalid(path.Child("localhostProfile"), localhostProfileVal, "localhostProfile must be a non-empty string"))
		}
	default:
		allErrs = append(allErrs, field.NotSupported(path.Child("type"), typeVal, []string{"RuntimeDefault", "Localhost"}))
	}
	return allErrs
}

func validatePodSecurityContextValues(m map[string]interface{}, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if value, found := m["runAsNonRoot"]; found {
		b, ok := value.(bool)
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("runAsNonRoot"), value, "runAsNonRoot must be a boolean"))
		} else if !b {
			allErrs = append(allErrs, field.Forbidden(path.Child("runAsNonRoot"), "runAsNonRoot cannot be set to false"))
		}
	}
	if value, found := m["runAsUser"]; found {
		allErrs = append(allErrs, validateRunAsUser(path.Child("runAsUser"), value)...)
	}
	if value, found := m["runAsGroup"]; found {
		allErrs = append(allErrs, validateNonNegativeInt64(path.Child("runAsGroup"), value)...)
	}
	if value, found := m["fsGroup"]; found {
		allErrs = append(allErrs, validateNonNegativeInt64(path.Child("fsGroup"), value)...)
	}
	if value, found := m["supplementalGroups"]; found {
		groups, ok := value.([]interface{})
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("supplementalGroups"), value, "supplementalGroups must be an array of integers"))
		} else {
			for i, groupVal := range groups {
				allErrs = append(allErrs, validateNonNegativeInt64(path.Child("supplementalGroups").Index(i), groupVal)...)
			}
		}
	}
	if value, found := m["fsGroupChangePolicy"]; found {
		policy, ok := value.(string)
		if !ok || policy == "" {
			allErrs = append(allErrs, field.Invalid(path.Child("fsGroupChangePolicy"), value, "fsGroupChangePolicy must be a non-empty string"))
		} else if policy != "Always" && policy != "OnRootMismatch" {
			allErrs = append(allErrs, field.NotSupported(path.Child("fsGroupChangePolicy"), policy, []string{"Always", "OnRootMismatch"}))
		}
	}
	return allErrs
}

func validateContainerSecurityContextValues(m map[string]interface{}, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if value, found := m["runAsNonRoot"]; found {
		b, ok := value.(bool)
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("runAsNonRoot"), value, "runAsNonRoot must be a boolean"))
		} else if !b {
			allErrs = append(allErrs, field.Forbidden(path.Child("runAsNonRoot"), "runAsNonRoot cannot be set to false"))
		}
	}
	if value, found := m["allowPrivilegeEscalation"]; found {
		b, ok := value.(bool)
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("allowPrivilegeEscalation"), value, "allowPrivilegeEscalation must be a boolean"))
		} else if b {
			allErrs = append(allErrs, field.Forbidden(path.Child("allowPrivilegeEscalation"), "allowPrivilegeEscalation cannot be set to true"))
		}
	}
	if value, found := m["readOnlyRootFilesystem"]; found {
		// One-way, like every other knob in this allow-list. Whether an agent
		// may write to its root filesystem is provider-owned
		// (AgentProviderConfig.spec.capabilities.writableRootFilesystem),
		// precisely so a deployment author cannot weaken the posture their
		// framework declared. Permitting false here reopened that hole: the
		// override is merged AFTER the hardened default is set, so it wins.
		b, ok := value.(bool)
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("readOnlyRootFilesystem"), value, "readOnlyRootFilesystem must be a boolean"))
		} else if !b {
			allErrs = append(allErrs, field.Forbidden(path.Child("readOnlyRootFilesystem"),
				"readOnlyRootFilesystem cannot be set to false; a writable root filesystem is provider-owned, set capabilities.writableRootFilesystem on the framework's AgentProviderConfig instead"))
		}
	}
	if value, found := m["runAsUser"]; found {
		allErrs = append(allErrs, validateRunAsUser(path.Child("runAsUser"), value)...)
	}
	if value, found := m["runAsGroup"]; found {
		allErrs = append(allErrs, validateNonNegativeInt64(path.Child("runAsGroup"), value)...)
	}
	return allErrs
}

func validateNonNegativeInt64(path *field.Path, value interface{}) field.ErrorList {
	number, ok := value.(json.Number)
	if !ok {
		return field.ErrorList{field.Invalid(path, value, "must be a non-negative 64-bit integer")}
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || parsed < 0 {
		return field.ErrorList{field.Invalid(path, value, "must be a non-negative 64-bit integer")}
	}
	return nil
}

// validateRunAsUser requires a strictly positive UID. Every rendered container
// keeps runAsNonRoot=true and overrides may not disable it, so runAsUser=0
// (root) would be admitted here but rejected by the kubelet at pod start.
func validateRunAsUser(path *field.Path, value interface{}) field.ErrorList {
	if errs := validateNonNegativeInt64(path, value); len(errs) > 0 {
		return errs
	}
	if number, ok := value.(json.Number); ok {
		parsed, _ := strconv.ParseInt(number.String(), 10, 64)
		if parsed == 0 {
			return field.ErrorList{field.Forbidden(path, "runAsUser cannot be 0 (root); runAsNonRoot is always enforced")}
		}
	}
	return nil
}
