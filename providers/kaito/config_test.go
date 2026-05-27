package kaito

import (
	"context"
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
		if spec.Capabilities.Engines[i] != e {
			t.Errorf("engine[%d]: expected %s, got %s", i, e, spec.Capabilities.Engines[i])
		}
	}

	if len(spec.Capabilities.ServingModes) != 1 || spec.Capabilities.ServingModes[0] != airunwayv1alpha1.ServingModeAggregated {
		t.Errorf("expected only aggregated serving mode")
	}

	if !spec.Capabilities.CPUSupport {
		t.Error("expected CPU support to be true")
	}
	if !spec.Capabilities.GPUSupport {
		t.Error("expected GPU support to be true")
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
