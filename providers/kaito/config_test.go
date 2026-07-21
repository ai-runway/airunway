package kaito

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

func TestGetProviderConfigSpec(t *testing.T) {
	spec := GetProviderConfigSpec()

	if spec.Capabilities == nil {
		t.Fatal("capabilities should not be nil")
	}

	expectedEngines := []airunwayv1alpha1.EngineType{
		airunwayv1alpha1.EngineTypeVLLM,
		airunwayv1alpha1.EngineTypeLlamaCpp,
	}
	if len(spec.Capabilities.Engines) != len(expectedEngines) {
		t.Fatalf("expected %d engines, got %d", len(expectedEngines), len(spec.Capabilities.Engines))
	}
	for i, e := range expectedEngines {
		if spec.Capabilities.Engines[i].Name != e {
			t.Errorf("engine[%d]: expected %s, got %s", i, e, spec.Capabilities.Engines[i].Name)
		}
	}

	// Verify per-engine capabilities
	vllmCap := spec.Capabilities.GetEngineCapability(airunwayv1alpha1.EngineTypeVLLM)
	if vllmCap == nil {
		t.Fatal("expected vllm engine capability")
	}
	if !vllmCap.GPUSupport {
		t.Error("expected vllm GPU support to be true")
	}
	if vllmCap.CPUSupport {
		t.Error("expected vllm CPU support to be false")
	}
	if len(vllmCap.ServingModes) != 1 || vllmCap.ServingModes[0] != airunwayv1alpha1.ServingModeAggregated {
		t.Errorf("expected vllm to support only aggregated serving mode")
	}

	llamaCap := spec.Capabilities.GetEngineCapability(airunwayv1alpha1.EngineTypeLlamaCpp)
	if llamaCap == nil {
		t.Fatal("expected llamacpp engine capability")
	}
	if !llamaCap.GPUSupport {
		t.Error("expected llamacpp GPU support to be true")
	}
	if !llamaCap.CPUSupport {
		t.Error("expected llamacpp CPU support to be true")
	}
	if len(llamaCap.ServingModes) != 1 || llamaCap.ServingModes[0] != airunwayv1alpha1.ServingModeAggregated {
		t.Errorf("expected llamacpp to support only aggregated serving mode")
	}

	if len(spec.SelectionRules) != 2 {
		t.Fatalf("expected 2 selection rules, got %d", len(spec.SelectionRules))
	}
	if spec.SelectionRules[0].Priority != 100 {
		t.Errorf("expected first rule priority 100, got %d", spec.SelectionRules[0].Priority)
	}
}

func TestGetInstallationInfo(t *testing.T) {
	info := GetInstallationInfo()
	if info == nil {
		t.Fatal("expected non-nil installation info")
	}
	if info.Description == "" {
		t.Error("expected non-empty description")
	}
	if info.DefaultNamespace != "kaito-workspace" {
		t.Errorf("expected defaultNamespace 'kaito-workspace', got %s", info.DefaultNamespace)
	}
	if len(info.HelmRepos) != 1 {
		t.Fatalf("expected 1 helm repo, got %d", len(info.HelmRepos))
	}
	if len(info.HelmCharts) != 1 {
		t.Fatalf("expected 1 helm chart, got %d", len(info.HelmCharts))
	}
	if len(info.Steps) != 3 {
		t.Fatalf("expected 3 installation steps, got %d", len(info.Steps))
	}
}

func TestNewProviderConfigManager(t *testing.T) {
	mgr := NewProviderConfigManager(nil, nil)
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}
}

func TestProviderConstants(t *testing.T) {
	if ProviderConfigName != "kaito" {
		t.Errorf("expected provider config name 'kaito', got %s", ProviderConfigName)
	}
	if ProviderVersion != "kaito-provider:v0.1.0" {
		t.Errorf("expected provider version 'kaito-provider:v0.1.0', got %s", ProviderVersion)
	}
}

func TestRegisterNew(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)

	c := newFakeClientWithWorkspace(scheme)
	mgr := NewProviderConfigManager(c, c)

	err := mgr.Register(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegisterExisting(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)

	existing := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: ProviderConfigName},
	}

	c := newFakeClientWithWorkspace(scheme, existing)
	mgr := NewProviderConfigManager(c, c)

	err := mgr.Register(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnregister(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)

	existing := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: ProviderConfigName},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).WithStatusSubresource(existing).Build()
	mgr := NewProviderConfigManager(c, c)

	err := mgr.Unregister(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStartHeartbeat(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)

	existing := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: ProviderConfigName},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).WithStatusSubresource(existing).Build()
	mgr := NewProviderConfigManager(c, c)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.StartHeartbeat(ctx)
	// Cancel immediately to stop the goroutine
	cancel()
}

func TestUpdateStatusFromProbe_HealthyPath(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)

	config := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: ProviderConfigName},
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaito-workspace",
			Namespace: "kaito-workspace",
			Labels:    map[string]string{"app.kubernetes.io/name": "workspace"},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}

	c := newFakeClientWithWorkspace(scheme, config, deploy)
	mgr := NewProviderConfigManager(c, c)
	if err := mgr.UpdateStatusFromProbe(context.Background()); err != nil {
		t.Fatalf("UpdateStatusFromProbe: %v", err)
	}

	got := &airunwayv1alpha1.InferenceProviderConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: ProviderConfigName}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Status.Ready {
		t.Error("expected Ready=true")
	}
	cond := findCondition(got.Status.Conditions, "UpstreamReady")
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonUpstreamHealthy {
		t.Errorf("unexpected UpstreamReady condition: %+v", cond)
	}
}

func TestMarkUnregistered(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)

	config := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: ProviderConfigName},
		Status:     airunwayv1alpha1.InferenceProviderConfigStatus{Ready: true},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(config).
		WithStatusSubresource(config).
		Build()

	mgr := NewProviderConfigManager(c, c)
	if err := mgr.MarkUnregistered(context.Background()); err != nil {
		t.Fatalf("MarkUnregistered: %v", err)
	}

	got := &airunwayv1alpha1.InferenceProviderConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: ProviderConfigName}, got)
	if got.Status.Ready {
		t.Error("expected Ready=false")
	}
	cond := findCondition(got.Status.Conditions, "UpstreamReady")
	if cond == nil || cond.Reason != ReasonUnregistered {
		t.Errorf("unexpected UpstreamReady condition: %+v", cond)
	}
}

func findCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

// newFakeClientWithWorkspace builds a fake client with the Workspace GVK registered
// so simpleMapper (from upstream_health_test.go) recognises it during probe calls.
func newFakeClientWithWorkspace(scheme *runtime.Scheme, objs ...client.Object) client.Client {
	// Register workspace GVK so the simpleMapper finds it
	gvk := schema.GroupVersionKind{Group: "kaito.sh", Version: "v1beta1", Kind: "Workspace"}
	scheme.AddKnownTypeWithName(gvk, &metav1.PartialObjectMetadata{})
	gvkList := schema.GroupVersionKind{Group: "kaito.sh", Version: "v1beta1", Kind: "WorkspaceList"}
	scheme.AddKnownTypeWithName(gvkList, &metav1.PartialObjectMetadataList{})
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Group: "kaito.sh", Version: "v1beta1"})

	mapper := &simpleMapper{scheme: scheme}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(mapper).
		WithObjects(objs...).
		WithStatusSubresource(&airunwayv1alpha1.InferenceProviderConfig{}).
		Build()
}

func TestBuildAnnotationsIncludesDiscoveryMetadata(t *testing.T) {
	annotations, err := buildAnnotations()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	requiredKeys := []string{
		airunwayv1alpha1.AnnotationDisplayName,
		airunwayv1alpha1.AnnotationDescription,
		airunwayv1alpha1.AnnotationDefaultNamespace,
		airunwayv1alpha1.AnnotationDocumentationURL,
		airunwayv1alpha1.AnnotationCapabilities,
		airunwayv1alpha1.AnnotationHealth,
		airunwayv1alpha1.AnnotationInstallation,
		airunwayv1alpha1.AnnotationDocumentation,
	}
	for _, key := range requiredKeys {
		if annotations[key] == "" {
			t.Fatalf("expected annotation %s to be set", key)
		}
	}
	if annotations[airunwayv1alpha1.AnnotationDisplayName] != "KAITO" {
		t.Fatalf("expected KAITO display name, got %q", annotations[airunwayv1alpha1.AnnotationDisplayName])
	}
	if annotations[airunwayv1alpha1.AnnotationDefaultNamespace] != "kaito-workspace" {
		t.Fatalf("expected kaito-workspace default namespace, got %q", annotations[airunwayv1alpha1.AnnotationDefaultNamespace])
	}

	var capabilities airunwayv1alpha1.ProviderCapabilities
	if err := json.Unmarshal([]byte(annotations[airunwayv1alpha1.AnnotationCapabilities]), &capabilities); err != nil {
		t.Fatalf("failed to decode capabilities annotation: %v", err)
	}
	if capabilities.GetEngineCapability(airunwayv1alpha1.EngineTypeLlamaCpp) == nil {
		t.Fatalf("expected annotated llamacpp capability, got %+v", capabilities.Engines)
	}

	var health struct {
		CRDs []struct {
			Name string `json:"name"`
		} `json:"crds"`
		OperatorPods []struct {
			Selectors []string `json:"selectors"`
		} `json:"operatorPods"`
	}
	if err := json.Unmarshal([]byte(annotations[airunwayv1alpha1.AnnotationHealth]), &health); err != nil {
		t.Fatalf("failed to decode health annotation: %v", err)
	}
	if len(health.CRDs) != 1 || health.CRDs[0].Name != "workspaces.kaito.sh" {
		t.Fatalf("expected KAITO CRD health probe, got %+v", health.CRDs)
	}
	if len(health.OperatorPods) < 2 {
		t.Fatalf("expected namespace and cross-namespace KAITO operator probes, got %+v", health.OperatorPods)
	}
}
