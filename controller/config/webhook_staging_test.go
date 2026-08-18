package config_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controllerpkg "github.com/ai-runway/airunway/controller/internal/controller"
)

const (
	agentDeploymentMutatingWebhook   = "magentdeployment-v1alpha1.kb.io"
	modelDeploymentMutatingWebhook   = "mmodeldeployment-v1alpha1.kb.io"
	agentDeploymentValidatingWebhook = "vagentdeployment-v1alpha1.kb.io"
	modelDeploymentValidatingWebhook = "vmodeldeployment-v1alpha1.kb.io"
	upgradeGuardConfiguration        = "airunway-agentdeployment-upgrade-guard"
	upgradeGuardWebhook              = "agentdeployment-upgrade-guard.airunway.ai"
	sharedMutatingConfiguration      = "airunway-mutating-webhook-configuration"
	sharedValidatingConfiguration    = "airunway-validating-webhook-configuration"
)

type renderedAdmission struct {
	mutating   map[string]*admissionregistrationv1.MutatingWebhookConfiguration
	validating map[string]*admissionregistrationv1.ValidatingWebhookConfiguration
}

func TestAgentDeploymentWebhookStaging(t *testing.T) {
	defaultAdmission := renderAdmission(t, "default")
	activatedAdmission := renderAdmission(t, "agentdeployment-webhook")
	guardAdmission := renderAdmission(t, "agentdeployment-webhook-guard")

	defaultMutating := requireMutatingConfiguration(t, defaultAdmission, sharedMutatingConfiguration)
	activatedMutating := requireMutatingConfiguration(t, activatedAdmission, sharedMutatingConfiguration)
	if mutatingWebhookByName(defaultMutating, agentDeploymentMutatingWebhook) != nil {
		t.Fatalf("default rolling-upgrade bundle must not register %q", agentDeploymentMutatingWebhook)
	}
	if mutatingWebhookByName(defaultMutating, modelDeploymentMutatingWebhook) == nil {
		t.Fatalf("default rolling-upgrade bundle must retain %q", modelDeploymentMutatingWebhook)
	}

	agentMutator := mutatingWebhookByName(activatedMutating, agentDeploymentMutatingWebhook)
	if agentMutator == nil {
		t.Fatalf("post-rollout bundle must register %q", agentDeploymentMutatingWebhook)
	}
	if agentMutator.FailurePolicy == nil || *agentMutator.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatalf("post-rollout webhook failurePolicy = %v, want Fail", agentMutator.FailurePolicy)
	}
	if agentMutator.ReinvocationPolicy == nil ||
		*agentMutator.ReinvocationPolicy != admissionregistrationv1.IfNeededReinvocationPolicy {
		t.Fatalf("post-rollout webhook reinvocationPolicy = %v, want IfNeeded", agentMutator.ReinvocationPolicy)
	}
	if agentMutator.ClientConfig.Service == nil || agentMutator.ClientConfig.Service.Path == nil ||
		*agentMutator.ClientConfig.Service.Path != "/mutate-airunway-ai-v1alpha1-agentdeployment" {
		t.Fatalf("post-rollout webhook service path = %v, want AgentDeployment mutating route", agentMutator.ClientConfig.Service)
	}
	if agentMutator.ClientConfig.Service.Name != "airunway-webhook-service" ||
		agentMutator.ClientConfig.Service.Namespace != "airunway-system" {
		t.Fatalf("post-rollout webhook service = %s/%s, want airunway-system/airunway-webhook-service",
			agentMutator.ClientConfig.Service.Namespace, agentMutator.ClientConfig.Service.Name)
	}

	defaultMutatingNames := mutatingWebhookNames(defaultMutating)
	activatedMutatingNames := mutatingWebhookNames(activatedMutating)
	wantActivatedMutatingNames := append(append([]string(nil), defaultMutatingNames...), agentDeploymentMutatingWebhook)
	sort.Strings(wantActivatedMutatingNames)
	if !reflect.DeepEqual(activatedMutatingNames, wantActivatedMutatingNames) {
		t.Fatalf("post-rollout mutating webhook names = %v, want default names plus %q (%v)",
			activatedMutatingNames, agentDeploymentMutatingWebhook, wantActivatedMutatingNames)
	}

	defaultValidating := requireValidatingConfiguration(t, defaultAdmission, sharedValidatingConfiguration)
	activatedValidating := requireValidatingConfiguration(t, activatedAdmission, sharedValidatingConfiguration)
	if validatingWebhookByName(defaultValidating, agentDeploymentValidatingWebhook) != nil {
		t.Fatalf("default rolling-upgrade bundle must not register side-effecting %q", agentDeploymentValidatingWebhook)
	}
	if validatingWebhookByName(defaultValidating, modelDeploymentValidatingWebhook) == nil {
		t.Fatalf("default rolling-upgrade bundle must retain %q", modelDeploymentValidatingWebhook)
	}
	agentValidator := validatingWebhookByName(activatedValidating, agentDeploymentValidatingWebhook)
	if agentValidator == nil {
		t.Fatalf("post-rollout bundle must register %q", agentDeploymentValidatingWebhook)
	}
	if agentValidator.FailurePolicy == nil || *agentValidator.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatalf("post-rollout validator failurePolicy = %v, want Fail", agentValidator.FailurePolicy)
	}
	if agentValidator.ClientConfig.Service == nil || agentValidator.ClientConfig.Service.Path == nil ||
		*agentValidator.ClientConfig.Service.Path != "/validate-airunway-ai-v1alpha1-agentdeployment" {
		t.Fatalf("post-rollout validator service path = %v, want AgentDeployment validating route", agentValidator.ClientConfig.Service)
	}

	defaultValidatingNames := validatingWebhookNames(defaultValidating)
	activatedValidatingNames := validatingWebhookNames(activatedValidating)
	wantActivatedValidatingNames := append(append([]string(nil), defaultValidatingNames...), agentDeploymentValidatingWebhook)
	sort.Strings(wantActivatedValidatingNames)
	if !reflect.DeepEqual(activatedValidatingNames, wantActivatedValidatingNames) {
		t.Fatalf("post-rollout validating webhook names = %v, want default names plus %q (%v)",
			activatedValidatingNames, agentDeploymentValidatingWebhook, wantActivatedValidatingNames)
	}
	if _, ok := defaultAdmission.mutating[upgradeGuardConfiguration]; ok {
		t.Fatalf("default bundle must not own the separately applied %q", upgradeGuardConfiguration)
	}
	if _, ok := activatedAdmission.mutating[upgradeGuardConfiguration]; ok {
		t.Fatalf("activation bundle must not remove or replace %q before its configurations apply successfully", upgradeGuardConfiguration)
	}
	if _, ok := guardAdmission.validating[upgradeGuardConfiguration]; ok {
		t.Fatalf("upgrade guard must run during mutating admission before side-effecting validators")
	}

	guard := requireMutatingConfiguration(t, guardAdmission, upgradeGuardConfiguration)
	if len(guard.Webhooks) != 1 || guard.Webhooks[0].Name != upgradeGuardWebhook {
		t.Fatalf("upgrade guard webhooks = %v, want only %q", mutatingWebhookNames(guard), upgradeGuardWebhook)
	}
	guardWebhook := &guard.Webhooks[0]
	if guardWebhook.FailurePolicy == nil || *guardWebhook.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatalf("upgrade guard failurePolicy = %v, want Fail", guardWebhook.FailurePolicy)
	}
	if guardWebhook.SideEffects == nil || *guardWebhook.SideEffects != admissionregistrationv1.SideEffectClassNone {
		t.Fatalf("upgrade guard sideEffects = %v, want None", guardWebhook.SideEffects)
	}
	if guardWebhook.TimeoutSeconds == nil || *guardWebhook.TimeoutSeconds != 1 {
		t.Fatalf("upgrade guard timeoutSeconds = %v, want 1", guardWebhook.TimeoutSeconds)
	}
	if guardWebhook.ClientConfig.Service == nil ||
		guardWebhook.ClientConfig.Service.Name != "airunway-agentdeployment-upgrade-guard" ||
		guardWebhook.ClientConfig.Service.Namespace != "airunway-system" ||
		guardWebhook.ClientConfig.Service.Path == nil ||
		*guardWebhook.ClientConfig.Service.Path != "/deny-agentdeployment-writes-during-controller-rollout" {
		t.Fatalf("upgrade guard service = %v, want unresolved airunway-system/airunway-agentdeployment-upgrade-guard",
			guardWebhook.ClientConfig.Service)
	}
	if len(guardWebhook.Rules) != 1 {
		t.Fatalf("upgrade guard rules = %d, want 1", len(guardWebhook.Rules))
	}
	rule := guardWebhook.Rules[0]
	if !reflect.DeepEqual(rule.Operations, []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
	}) || !reflect.DeepEqual(rule.Rule.APIGroups, []string{"airunway.ai"}) ||
		!reflect.DeepEqual(rule.Rule.APIVersions, []string{"v1alpha1"}) ||
		!reflect.DeepEqual(rule.Rule.Resources, []string{"agentdeployments"}) {
		t.Fatalf("upgrade guard rule = %#v, want only AgentDeployment CREATE and UPDATE", rule)
	}
	if rule.Rule.Scope == nil || *rule.Rule.Scope != admissionregistrationv1.NamespacedScope {
		t.Fatalf("upgrade guard scope = %v, want Namespaced", rule.Rule.Scope)
	}

	scheme := runtime.NewScheme()
	if err := admissionregistrationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add admission registration types to scheme: %v", err)
	}
	defaultOnly := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaultValidating.DeepCopy()).Build()
	if err := controllerpkg.VerifyAgentCredentialAdmission(context.Background(), defaultOnly); err == nil {
		t.Fatal("default rolling-upgrade bundle without its separately applied guard must fail credential admission verification")
	}
	guarded := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaultValidating.DeepCopy(), guard.DeepCopy()).Build()
	if err := controllerpkg.VerifyAgentCredentialAdmission(context.Background(), guarded); err != nil {
		t.Fatalf("rendered phase-two default bundle plus upgrade guard must pass credential admission verification: %v", err)
	}
	activated := fake.NewClientBuilder().WithScheme(scheme).WithObjects(activatedValidating.DeepCopy()).Build()
	if err := controllerpkg.VerifyAgentCredentialAdmission(context.Background(), activated); err != nil {
		t.Fatalf("rendered post-rollout validator must pass credential admission verification: %v", err)
	}
}

func TestManagerSecretRBAC(t *testing.T) {
	contents, err := os.ReadFile("rbac/role.yaml")
	if err != nil {
		t.Fatalf("read generated manager role: %v", err)
	}
	jsonContents, err := yaml.ToJSON(contents)
	if err != nil {
		t.Fatalf("convert generated manager role to JSON: %v", err)
	}
	var role rbacv1.ClusterRole
	if err := json.Unmarshal(jsonContents, &role); err != nil {
		t.Fatalf("decode generated manager role: %v", err)
	}

	// The remaining verbs each have a concrete controller call path: get for
	// credential and ownership checks; create for provider-owned and signing-key
	// Secrets; patch for owned keyless Secrets and signing-key initialization;
	// list/watch for the metadata-only credential rotation watch; and delete for
	// exact-owned, UID-preconditioned ingress-token cleanup. Secret update has no
	// call path and must not return through marker coalescing.
	want := []string{"create", "delete", "get", "list", "patch", "watch"}
	gotSet := make(map[string]struct{})
	for _, rule := range role.Rules {
		if !containsString(rule.APIGroups, "") || !containsString(rule.Resources, "secrets") {
			continue
		}
		for _, verb := range rule.Verbs {
			gotSet[verb] = struct{}{}
		}
	}
	got := make([]string, 0, len(gotSet))
	for verb := range gotSet {
		got = append(got, verb)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generated manager Secret verbs = %v, want only required verbs %v", got, want)
	}
}

func TestManagerAdmissionConfigurationRBAC(t *testing.T) {
	contents, err := os.ReadFile("rbac/role.yaml")
	if err != nil {
		t.Fatalf("read generated manager role: %v", err)
	}
	jsonContents, err := yaml.ToJSON(contents)
	if err != nil {
		t.Fatalf("convert generated manager role to JSON: %v", err)
	}
	var role rbacv1.ClusterRole
	if err := json.Unmarshal(jsonContents, &role); err != nil {
		t.Fatalf("decode generated manager role: %v", err)
	}

	want := map[string]bool{
		"mutatingwebhookconfigurations":   false,
		"validatingwebhookconfigurations": false,
	}
	for _, rule := range role.Rules {
		if !containsString(rule.APIGroups, "admissionregistration.k8s.io") {
			continue
		}
		for resource := range want {
			if !containsString(rule.Resources, resource) {
				continue
			}
			if !reflect.DeepEqual(rule.Verbs, []string{"get"}) {
				t.Fatalf("generated manager %s verbs = %v, want only get", resource, rule.Verbs)
			}
			want[resource] = true
		}
	}
	for resource, found := range want {
		if !found {
			t.Fatalf("generated manager role does not allow get on %s", resource)
		}
	}
}

func TestControllerDeployOrdersAdmissionGuard(t *testing.T) {
	makefile, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatalf("read controller Makefile: %v", err)
	}
	sequence := makeTargetRecipe(t, makefile, "deploy") + "\n" +
		makeTargetRecipe(t, makefile, "activate-agentdeployment-webhook")
	wantInOrder := []string{
		`"$(KUSTOMIZE)" build config/agentdeployment-webhook-guard | "$(KUBECTL)" apply -f -`,
		`"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -`,
		`"$(KUBECTL)" rollout restart deployment/airunway-controller-manager`,
		`$(MAKE) activate-agentdeployment-webhook`,
		`"$(KUBECTL)" rollout status deployment/airunway-controller-manager`,
		`"$(KUSTOMIZE)" build config/agentdeployment-webhook | "$(KUBECTL)" apply -f -`,
		`$(MAKE) wait-agentdeployment-webhook-ca`,
		`"$(KUSTOMIZE)" build config/agentdeployment-webhook-guard | "$(KUBECTL)" delete --ignore-not-found=true -f -`,
	}
	position := -1
	for _, command := range wantInOrder {
		next := strings.Index(sequence[position+1:], command)
		if next < 0 {
			t.Fatalf("staged deploy recipes do not contain %q\n%s", command, sequence)
		}
		position += next + 1
	}

	caWaitRecipe := makeTargetRecipe(t, makefile, "wait-agentdeployment-webhook-ca")
	caWaitsInOrder := []string{
		`"$(KUBECTL)" wait secret/airunway-webhook-server-cert`,
		`mutatingwebhookconfiguration/airunway-mutating-webhook-configuration`,
		`validatingwebhookconfiguration/airunway-validating-webhook-configuration`,
	}
	position = -1
	for _, command := range caWaitsInOrder {
		next := strings.Index(caWaitRecipe[position+1:], command)
		if next < 0 {
			t.Fatalf("webhook CA wait recipe does not contain %q\n%s", command, caWaitRecipe)
		}
		position += next + 1
	}
}

func TestDocumentedStagedCommandsFailClosed(t *testing.T) {
	documents := []string{
		"../../README.md",
		"../README.md",
		"../../deploy/README.md",
		"../../docs/versioning-upgrades.md",
		"../../docs/providers/vllm.md",
	}
	for _, document := range documents {
		document := document
		t.Run(filepath.Base(filepath.Dir(document))+"-"+filepath.Base(document), func(t *testing.T) {
			contents, err := os.ReadFile(document)
			if err != nil {
				t.Fatalf("read %s: %v", document, err)
			}
			stagedBlocks := 0
			blocks := strings.Split(string(contents), "```")
			for i := 1; i < len(blocks); i += 2 {
				block := blocks[i]
				guardApply := strings.Index(block, "kubectl apply -f")
				guardDelete := strings.Index(block, "kubectl delete")
				if guardApply < 0 || guardDelete < guardApply ||
					strings.Count(block, "agentdeployment-webhook-guard.yaml") < 2 {
					continue
				}
				stagedBlocks++
				failFast := strings.Index(block, "set -euo pipefail")
				if failFast < 0 || failFast > guardApply {
					t.Fatalf("staged command block in %s must enable fail-fast shell mode before applying the guard:\n%s", document, block)
				}
				rolloutStatus := strings.Index(block, "kubectl rollout status deployment/airunway-controller-manager")
				if rolloutStatus < 0 || rolloutStatus > guardDelete {
					t.Fatalf("staged command block in %s must wait for the controller before removing the guard:\n%s", document, block)
				}
				if rolloutUndo := strings.Index(block, "kubectl rollout undo deployment/airunway-controller-manager"); rolloutUndo >= 0 {
					if rolloutUndo > rolloutStatus {
						t.Fatalf("rollback command block in %s must trigger rollback before waiting for it:\n%s", document, block)
					}
					finalRolloutStatus := strings.LastIndex(block, "kubectl rollout status deployment/airunway-controller-manager")
					caSecretWait := strings.Index(block, "kubectl wait secret/airunway-webhook-server-cert")
					caRead := strings.Index(block, "AIRUNWAY_WEBHOOK_CA_BUNDLE=\"$(kubectl get secret/airunway-webhook-server-cert")
					validatingCAWait := strings.Index(block, "kubectl wait validatingwebhookconfiguration/airunway-validating-webhook-configuration")
					if finalRolloutStatus <= rolloutStatus || caSecretWait < finalRolloutStatus || caRead < caSecretWait ||
						validatingCAWait < caRead || guardDelete < validatingCAWait {
						t.Fatalf("rollback command block in %s must wait for cert-controller to trust the restored validator before removing the guard:\n%s", document, block)
					}
					continue
				}
				rolloutRestart := strings.Index(block, "kubectl rollout restart deployment/airunway-controller-manager")
				if rolloutRestart < 0 || rolloutRestart > rolloutStatus {
					t.Fatalf("staged command block in %s must force a new controller ReplicaSet before waiting for rollout:\n%s", document, block)
				}
				secondApply := strings.Index(block[guardApply+len("kubectl apply -f"):], "kubectl apply -f")
				if secondApply < 0 || guardApply+len("kubectl apply -f")+secondApply > rolloutRestart {
					t.Fatalf("staged command block in %s must apply the controller before restarting it:\n%s", document, block)
				}
				activationApply := strings.Index(block, "agentdeployment-webhook.yaml")
				caSecretWait := strings.Index(block, "kubectl wait secret/airunway-webhook-server-cert")
				caRead := strings.Index(block, "AIRUNWAY_WEBHOOK_CA_BUNDLE=\"$(kubectl get secret/airunway-webhook-server-cert")
				mutatingCAWait := strings.Index(block, "kubectl wait mutatingwebhookconfiguration/airunway-mutating-webhook-configuration")
				validatingCAWait := strings.Index(block, "kubectl wait validatingwebhookconfiguration/airunway-validating-webhook-configuration")
				if activationApply < 0 || caSecretWait < activationApply || caRead < caSecretWait ||
					mutatingCAWait < caRead || validatingCAWait < mutatingCAWait || guardDelete < validatingCAWait {
					t.Fatalf("staged command block in %s must wait for cert-controller to inject the trusted CA before removing the guard:\n%s", document, block)
				}
			}
			if stagedBlocks == 0 {
				t.Fatalf("found no staged guard apply/delete command block in %s", document)
			}
		})
	}
}

func renderAdmission(t *testing.T, configDir string) renderedAdmission {
	t.Helper()

	kustomize := os.Getenv("KUSTOMIZE")
	if kustomize == "" {
		kustomize = filepath.Join("..", "bin", "kustomize")
	}
	if _, err := os.Stat(kustomize); err != nil {
		t.Skipf("kustomize is not installed at %q; run make kustomize: %v", kustomize, err)
	}

	cmd := exec.Command(kustomize, "build", configDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kustomize build config/%s: %v\n%s", configDir, err, output)
	}

	result := renderedAdmission{
		mutating:   make(map[string]*admissionregistrationv1.MutatingWebhookConfiguration),
		validating: make(map[string]*admissionregistrationv1.ValidatingWebhookConfiguration),
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode config/%s output: %v", configDir, err)
		}
		switch object.GetKind() {
		case "MutatingWebhookConfiguration":
			var config admissionregistrationv1.MutatingWebhookConfiguration
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &config); err != nil {
				t.Fatalf("convert config/%s MutatingWebhookConfiguration: %v", configDir, err)
			}
			result.mutating[config.Name] = &config
		case "ValidatingWebhookConfiguration":
			var config admissionregistrationv1.ValidatingWebhookConfiguration
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &config); err != nil {
				t.Fatalf("convert config/%s ValidatingWebhookConfiguration: %v", configDir, err)
			}
			result.validating[config.Name] = &config
		}
	}
	return result
}

func requireMutatingConfiguration(t *testing.T, admission renderedAdmission, name string) *admissionregistrationv1.MutatingWebhookConfiguration {
	t.Helper()
	config := admission.mutating[name]
	if config == nil {
		t.Fatalf("rendered no MutatingWebhookConfiguration %q", name)
	}
	return config
}

func requireValidatingConfiguration(t *testing.T, admission renderedAdmission, name string) *admissionregistrationv1.ValidatingWebhookConfiguration {
	t.Helper()
	config := admission.validating[name]
	if config == nil {
		t.Fatalf("rendered no ValidatingWebhookConfiguration %q", name)
	}
	return config
}

func mutatingWebhookByName(config *admissionregistrationv1.MutatingWebhookConfiguration, name string) *admissionregistrationv1.MutatingWebhook {
	for i := range config.Webhooks {
		if config.Webhooks[i].Name == name {
			return &config.Webhooks[i]
		}
	}
	return nil
}

func validatingWebhookByName(config *admissionregistrationv1.ValidatingWebhookConfiguration, name string) *admissionregistrationv1.ValidatingWebhook {
	for i := range config.Webhooks {
		if config.Webhooks[i].Name == name {
			return &config.Webhooks[i]
		}
	}
	return nil
}

func mutatingWebhookNames(config *admissionregistrationv1.MutatingWebhookConfiguration) []string {
	names := make([]string, 0, len(config.Webhooks))
	for i := range config.Webhooks {
		names = append(names, config.Webhooks[i].Name)
	}
	sort.Strings(names)
	return names
}

func validatingWebhookNames(config *admissionregistrationv1.ValidatingWebhookConfiguration) []string {
	names := make([]string, 0, len(config.Webhooks))
	for i := range config.Webhooks {
		names = append(names, config.Webhooks[i].Name)
	}
	sort.Strings(names)
	return names
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func makeTargetRecipe(t *testing.T, makefile []byte, target string) string {
	t.Helper()
	lines := strings.Split(string(makefile), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, target+":") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("controller Makefile has no %q target", target)
	}
	var recipe []string
	for _, line := range lines[start:] {
		if strings.HasPrefix(line, "\t") || strings.TrimSpace(line) == "" {
			recipe = append(recipe, line)
			continue
		}
		break
	}
	return strings.Join(recipe, "\n")
}
