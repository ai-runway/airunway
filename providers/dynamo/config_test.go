package dynamo

import (
	"context"
	"encoding/json"
	"testing"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetProviderConfigSpec(t *testing.T) {
	spec := GetProviderConfigSpec()

	if spec.Capabilities == nil {
		t.Fatal("capabilities should not be nil")
	}

	expectedEngines := []airunwayv1alpha1.EngineType{
		airunwayv1alpha1.EngineTypeVLLM,
		airunwayv1alpha1.EngineTypeSGLang,
		airunwayv1alpha1.EngineTypeTRTLLM,
	}
	if len(spec.Capabilities.Engines) != len(expectedEngines) {
		t.Fatalf("expected %d engines, got %d", len(expectedEngines), len(spec.Capabilities.Engines))
	}

	if len(spec.Capabilities.ServingModes) != 2 {
		t.Fatalf("expected 2 serving modes, got %d", len(spec.Capabilities.ServingModes))
	}

	if spec.Capabilities.CPUSupport {
		t.Error("expected CPU support to be false")
	}
	if !spec.Capabilities.GPUSupport {
		t.Error("expected GPU support to be true")
	}

	if len(spec.SelectionRules) != 4 {
		t.Fatalf("expected 4 selection rules, got %d", len(spec.SelectionRules))
	}

	if spec.Capabilities.Gateway == nil {
		t.Fatal("gateway capabilities should not be nil")
	}
	if spec.Capabilities.Gateway.InferencePoolNamePattern != "{name}-pool" {
		t.Errorf("expected inference pool name pattern to be '{name}-pool', got %s", spec.Capabilities.Gateway.InferencePoolNamePattern)
	}
	if spec.Capabilities.Gateway.InferencePoolNamespace != "{namespace}" {
		t.Errorf("expected inference pool namespace to be '{namespace}', got %s", spec.Capabilities.Gateway.InferencePoolNamespace)
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
	if info.DefaultNamespace != "dynamo-system" {
		t.Errorf("expected defaultNamespace 'dynamo-system', got %s", info.DefaultNamespace)
	}
	if len(info.HelmCharts) != 1 {
		t.Fatalf("expected 1 helm chart, got %d", len(info.HelmCharts))
	}
	if info.HelmCharts[0].Chart != DynamoPlatformChartURL {
		t.Errorf("expected platform chart URL %q, got %q", DynamoPlatformChartURL, info.HelmCharts[0].Chart)
	}
	if info.HelmCharts[0].Values == nil || len(info.HelmCharts[0].Values.Raw) == 0 {
		t.Fatal("expected dynamo platform chart to include Helm values")
	}
	var values map[string]bool
	if err := json.Unmarshal(info.HelmCharts[0].Values.Raw, &values); err != nil {
		t.Fatalf("failed to decode Helm values: %v", err)
	}
	if !values["global.grove.install"] {
		t.Fatalf("expected global.grove.install=true, got %s", string(info.HelmCharts[0].Values.Raw))
	}
	if len(info.Steps) != 1 {
		t.Fatalf("expected 1 installation step, got %d", len(info.Steps))
	}
	if info.Steps[0].Command != "helm upgrade --install dynamo-platform "+DynamoPlatformChartURL+" --namespace dynamo-system --create-namespace --set-json global.grove.install=true" {
		t.Fatalf("unexpected installation command: %s", info.Steps[0].Command)
	}
}

func TestNewProviderConfigManager(t *testing.T) {
	mgr := NewProviderConfigManager(nil)
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}
}

func TestProviderConstants(t *testing.T) {
	if ProviderConfigName != "dynamo" {
		t.Errorf("expected provider config name 'dynamo', got %s", ProviderConfigName)
	}
	if ProviderVersion != "dynamo-provider:v0.2.0" {
		t.Errorf("expected provider version 'dynamo-provider:v0.2.0', got %s", ProviderVersion)
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

func TestRegisterAnnotatesInstallationMetadata(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&airunwayv1alpha1.InferenceProviderConfig{}).Build()
	mgr := NewProviderConfigManager(c)

	if err := mgr.Register(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	registered := &airunwayv1alpha1.InferenceProviderConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: ProviderConfigName}, registered); err != nil {
		t.Fatalf("failed to get registered provider config: %v", err)
	}

	if registered.Annotations[airunwayv1alpha1.AnnotationDocumentation] != ProviderDocumentation {
		t.Fatalf("expected documentation annotation %q, got %q", ProviderDocumentation, registered.Annotations[airunwayv1alpha1.AnnotationDocumentation])
	}

	var installation airunwayv1alpha1.InstallationInfo
	if err := json.Unmarshal([]byte(registered.Annotations[airunwayv1alpha1.AnnotationInstallation]), &installation); err != nil {
		t.Fatalf("failed to decode installation annotation: %v", err)
	}
	if len(installation.HelmCharts) != 1 {
		t.Fatalf("expected 1 annotated helm chart, got %d", len(installation.HelmCharts))
	}
	if installation.HelmCharts[0].Values == nil || len(installation.HelmCharts[0].Values.Raw) == 0 {
		t.Fatal("expected annotated Helm chart to include values")
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
			GroupVersion: DynamoAPIGroup + "/" + DynamoAPIVersion,
			APIResources: []metav1.APIResource{
				{Name: dynamoGraphDeploymentResource},
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

	updated := &airunwayv1alpha1.InferenceProviderConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: ProviderConfigName}, updated); err != nil {
		t.Fatalf("failed to get updated provider config: %v", err)
	}

	if !updated.Status.Ready {
		t.Fatal("expected provider status to be ready")
	}
	if updated.Status.Version != ProviderVersion {
		t.Fatalf("expected provider status version %q, got %q", ProviderVersion, updated.Status.Version)
	}
	if updated.Status.LastHeartbeat == nil {
		t.Fatal("expected provider status to include last heartbeat")
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
