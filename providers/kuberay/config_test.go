package kuberay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetProviderConfigSpec(t *testing.T) {
	spec := GetProviderConfigSpec()

	if spec.Capabilities == nil {
		t.Fatal("capabilities should not be nil")
	}

	if len(spec.Capabilities.Engines) != 1 {
		t.Fatalf("expected 1 engine, got %d", len(spec.Capabilities.Engines))
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
	if len(vllmCap.ServingModes) != 2 {
		t.Fatalf("expected vllm to support 2 serving modes, got %d", len(vllmCap.ServingModes))
	}
	if len(vllmCap.APIFormats) != 1 || vllmCap.APIFormats[0] != airunwayv1alpha1.APIFormatOpenAIChat {
		t.Errorf("expected vllm to support only openai-chat API format, got %v", vllmCap.APIFormats)
	}

	if len(spec.SelectionRules) != 1 {
		t.Fatalf("expected 1 selection rule, got %d", len(spec.SelectionRules))
	}
	if spec.SelectionRules[0].Priority != 80 {
		t.Errorf("expected priority 80, got %d", spec.SelectionRules[0].Priority)
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
	if info.DefaultNamespace != "ray-system" {
		t.Errorf("expected defaultNamespace 'ray-system', got %s", info.DefaultNamespace)
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
	mgr := NewProviderConfigManager(nil)
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}
}

func TestProviderConstants(t *testing.T) {
	if ProviderConfigName != "kuberay" {
		t.Errorf("expected provider config name 'kuberay', got %s", ProviderConfigName)
	}
	if !strings.HasPrefix(ProviderVersion, "kuberay-provider:") {
		t.Errorf("expected provider version to start with 'kuberay-provider:', got %s", ProviderVersion)
	}
}

func TestRegisterNew(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&airunwayv1alpha1.InferenceProviderConfig{}).Build()
	mgr := NewProviderConfigManager(c)

	err := mgr.Register(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegisterExisting(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)

	existing := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: ProviderConfigName},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).WithStatusSubresource(existing).Build()
	mgr := NewProviderConfigManager(c)

	err := mgr.Register(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckBackendCRDInstalledUsesDiscoveryFreshResults(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)

	discoveryClient := &fakediscovery.FakeDiscovery{
		Fake: &k8stesting.Fake{},
	}
	discoveryClient.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: RayAPIGroup + "/" + RayAPIVersion,
			APIResources: []metav1.APIResource{
				{Name: rayServiceResource},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := NewProviderConfigManager(c, discoveryClient)

	if !mgr.checkBackendCRDInstalled() {
		t.Fatal("expected backend CRD to be detected")
	}

	discoveryClient.Resources = []*metav1.APIResourceList{}

	if mgr.checkBackendCRDInstalled() {
		t.Fatal("expected backend CRD removal to be detected on the next check")
	}
}

func TestUpdateStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)

	existing := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: ProviderConfigName},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).WithStatusSubresource(existing).Build()
	mgr := NewProviderConfigManager(c)

	err := mgr.UpdateStatus(context.Background(), true)
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
	mgr := NewProviderConfigManager(c)

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
	mgr := NewProviderConfigManager(c)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.StartHeartbeat(ctx)
	cancel()
}

func TestUpdateStatusNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := NewProviderConfigManager(c)

	err := mgr.UpdateStatus(context.Background(), true)
	if err == nil {
		t.Fatal("expected error when config not found")
	}
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
	if annotations[airunwayv1alpha1.AnnotationDisplayName] != "KubeRay" {
		t.Fatalf("expected KubeRay display name, got %q", annotations[airunwayv1alpha1.AnnotationDisplayName])
	}
	if annotations[airunwayv1alpha1.AnnotationDefaultNamespace] != "ray-system" {
		t.Fatalf("expected ray-system default namespace, got %q", annotations[airunwayv1alpha1.AnnotationDefaultNamespace])
	}

	var capabilities airunwayv1alpha1.ProviderCapabilities
	if err := json.Unmarshal([]byte(annotations[airunwayv1alpha1.AnnotationCapabilities]), &capabilities); err != nil {
		t.Fatalf("failed to decode capabilities annotation: %v", err)
	}
	if capabilities.GetEngineCapability(airunwayv1alpha1.EngineTypeVLLM) == nil {
		t.Fatalf("expected annotated vllm capability, got %+v", capabilities.Engines)
	}

	var health struct {
		CRDs []struct {
			Name string `json:"name"`
		} `json:"crds"`
		OperatorPods []struct {
			Namespace string   `json:"namespace"`
			Selectors []string `json:"selectors"`
		} `json:"operatorPods"`
	}
	if err := json.Unmarshal([]byte(annotations[airunwayv1alpha1.AnnotationHealth]), &health); err != nil {
		t.Fatalf("failed to decode health annotation: %v", err)
	}
	if len(health.CRDs) != 1 || health.CRDs[0].Name != "rayservices.ray.io" {
		t.Fatalf("expected KubeRay CRD health probe, got %+v", health.CRDs)
	}
	if len(health.OperatorPods) != 2 {
		t.Fatalf("expected namespace and cross-namespace KubeRay operator probes, got %+v", health.OperatorPods)
	}
	if health.OperatorPods[0].Namespace != "ray-system" {
		t.Fatalf("expected namespaced KubeRay operator probe in ray-system, got %+v", health.OperatorPods[0])
	}
	if health.OperatorPods[1].Namespace != "" || len(health.OperatorPods[1].Selectors) != 1 || health.OperatorPods[1].Selectors[0] != "app.kubernetes.io/name=kuberay-operator" {
		t.Fatalf("expected cross-namespace KubeRay operator fallback, got %+v", health.OperatorPods[1])
	}
}
