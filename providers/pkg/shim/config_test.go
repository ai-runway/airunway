package shim

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type restMapperClient struct {
	client.Client
	mapper meta.RESTMapper
}

func (c restMapperClient) RESTMapper() meta.RESTMapper {
	return c.mapper
}

func TestRegisterProviderConfigCreatesMissingConfig(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}

	testClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	annotations := map[string]string{"airunway.ai/provider": "test-provider"}
	spec := airunwayv1alpha1.InferenceProviderConfigSpec{
		SelectionRules: []airunwayv1alpha1.SelectionRule{{Condition: "true", Priority: 42}},
	}

	if err := RegisterProviderConfig(context.Background(), testClient, "test-provider", annotations, spec); err != nil {
		t.Fatalf("RegisterProviderConfig() unexpected error: %v", err)
	}

	var stored airunwayv1alpha1.InferenceProviderConfig
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: "test-provider"}, &stored); err != nil {
		t.Fatalf("failed to fetch created config: %v", err)
	}

	if stored.Name != "test-provider" {
		t.Fatalf("stored config name = %q, want %q", stored.Name, "test-provider")
	}
	if got := stored.Annotations["airunway.ai/provider"]; got != "test-provider" {
		t.Fatalf("stored annotation = %q, want %q", got, "test-provider")
	}
	if len(stored.Spec.SelectionRules) != 1 || stored.Spec.SelectionRules[0].Priority != 42 {
		t.Fatalf("stored spec selection rules = %#v, want one rule with priority 42", stored.Spec.SelectionRules)
	}
}

func TestRegisterProviderConfigReturnsGetError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}

	testClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(
			ctx context.Context,
			cl client.WithWatch,
			key client.ObjectKey,
			obj client.Object,
			opts ...client.GetOption,
		) error {
			return errors.New("boom")
		},
	}).Build()

	err := RegisterProviderConfig(
		context.Background(),
		testClient,
		"broken-provider",
		nil,
		airunwayv1alpha1.InferenceProviderConfigSpec{},
	)
	if err == nil {
		t.Fatal("RegisterProviderConfig() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to get InferenceProviderConfig") {
		t.Fatalf("error = %q, want substring %q", err.Error(), "failed to get InferenceProviderConfig")
	}
}

func TestRegisterProviderConfigUpdatesExistingConfig(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}

	existing := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "existing-provider",
			Annotations: map[string]string{
				"existing": "keep",
			},
		},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			SelectionRules: []airunwayv1alpha1.SelectionRule{{Condition: "false", Priority: 1}},
		},
	}

	testClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	annotations := map[string]string{
		"existing": "overwritten",
		"new":      "value",
	}
	spec := airunwayv1alpha1.InferenceProviderConfigSpec{
		SelectionRules: []airunwayv1alpha1.SelectionRule{{Condition: "true", Priority: 99}},
	}

	err := RegisterProviderConfig(
		context.Background(),
		testClient,
		"existing-provider",
		annotations, spec)
	if err != nil {
		t.Fatalf("RegisterProviderConfig() unexpected error: %v", err)
	}

	var stored airunwayv1alpha1.InferenceProviderConfig
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: "existing-provider"}, &stored); err != nil {
		t.Fatalf("failed to fetch updated config: %v", err)
	}

	if got := stored.Annotations["existing"]; got != "overwritten" {
		t.Fatalf("stored existing annotation = %q, want %q", got, "overwritten")
	}
	if got := stored.Annotations["new"]; got != "value" {
		t.Fatalf("stored new annotation = %q, want %q", got, "value")
	}
	if len(stored.Spec.SelectionRules) != 1 || stored.Spec.SelectionRules[0].Priority != 99 {
		t.Fatalf("stored spec selection rules = %#v, want one rule with priority 99", stored.Spec.SelectionRules)
	}
}

func TestRetryStatusUpdateSuccessOnFirstAttempt(t *testing.T) {
	attempts := 0
	err := RetryStatusUpdate(context.Background(), 5, 10*time.Millisecond, func(ctx context.Context) error {
		attempts++
		return nil
	})
	if err != nil {
		t.Fatalf("RetryStatusUpdate() unexpected error: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempt count = %d, want 1", attempts)
	}
}

func TestRetryStatusUpdateEventualSuccess(t *testing.T) {
	attempts := 0
	err := RetryStatusUpdate(context.Background(), 5, time.Millisecond, func(ctx context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RetryStatusUpdate() unexpected error: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempt count = %d, want 3", attempts)
	}
}

func TestRetryStatusUpdateReturnsLastError(t *testing.T) {
	finalErr := errors.New("still failing")
	attempts := 0
	err := RetryStatusUpdate(context.Background(), 3, time.Millisecond, func(ctx context.Context) error {
		attempts++
		return finalErr
	})
	if !errors.Is(err, finalErr) {
		t.Fatalf("RetryStatusUpdate() error = %v, want %v", err, finalErr)
	}
	if attempts != 3 {
		t.Fatalf("attempt count = %d, want 3", attempts)
	}
}

func TestRetryStatusUpdateAttemptsFloorToOne(t *testing.T) {
	attempts := 0
	err := RetryStatusUpdate(context.Background(), 0, time.Millisecond, func(ctx context.Context) error {
		attempts++
		return nil
	})
	if err != nil {
		t.Fatalf("RetryStatusUpdate() unexpected error: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempt count = %d, want 1", attempts)
	}
}

func TestRetryStatusUpdateCanceledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := RetryStatusUpdate(ctx, 5, time.Second, func(ctx context.Context) error {
		attempts++
		cancel()
		return errors.New("retry me")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RetryStatusUpdate() error = %v, want context canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempt count = %d, want 1", attempts)
	}
}

func TestStartHeartbeatLoopInvokesTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ticks atomic.Int32
	done := make(chan struct{})

	StartHeartbeatLoop(ctx, 2*time.Millisecond, func(ctx context.Context) error {
		if ticks.Add(1) >= 2 {
			select {
			case <-done:
			default:
				close(done)
			}
			cancel()
		}
		return nil
	})

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for heartbeat ticks")
	}
}

func TestStartHeartbeatLoopContinuesAfterError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ticks atomic.Int32
	done := make(chan struct{})

	StartHeartbeatLoop(ctx, 2*time.Millisecond, func(ctx context.Context) error {
		n := ticks.Add(1)
		if n <= 2 {
			return errors.New("tick failed")
		}
		select {
		case <-done:
		default:
			close(done)
		}
		cancel()
		return nil
	})

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for post-error heartbeat tick")
	}

	if ticks.Load() < 3 {
		t.Fatalf("tick count = %d, want at least 3", ticks.Load())
	}
}

func TestStartHeartbeatLoopStopsWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ticks atomic.Int32
	StartHeartbeatLoop(ctx, time.Millisecond, func(ctx context.Context) error {
		ticks.Add(1)
		return nil
	})

	time.Sleep(20 * time.Millisecond)
	if ticks.Load() != 0 {
		t.Fatalf("tick count = %d, want 0", ticks.Load())
	}
}

func TestIsAPIResourceInstalledUsesDiscoveryFreshResults(t *testing.T) {
	discoveryClient := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	discoveryClient.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "ray.io/v1",
			APIResources: []metav1.APIResource{{Name: "rayservices"}},
		},
	}

	if !IsAPIResourceInstalled(nil, discoveryClient, "ray.io", "v1", "RayService", "rayservices") {
		t.Fatal("expected API resource to be detected via discovery")
	}

	discoveryClient.Resources = []*metav1.APIResourceList{}
	if IsAPIResourceInstalled(nil, discoveryClient, "ray.io", "v1", "RayService", "rayservices") {
		t.Fatal("expected API resource absence to be detected on fresh discovery read")
	}
}

func TestIsAPIResourceInstalledFallsBackToRESTMapper(t *testing.T) {
	baseClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "ray.io", Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Group: "ray.io", Version: "v1", Kind: "RayService"}, meta.RESTScopeNamespace)

	if !IsAPIResourceInstalled(
		restMapperClient{Client: baseClient, mapper: mapper},
		nil,
		"ray.io",
		"v1",
		"RayService",
		"rayservices",
	) {
		t.Fatal("expected API resource mapping to be detected via RESTMapper fallback")
	}
}

func TestIsAPIResourceInstalledRESTMapperFailures(t *testing.T) {
	baseClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	if IsAPIResourceInstalled(
		restMapperClient{Client: baseClient, mapper: nil},
		nil,
		"ray.io",
		"v1",
		"RayService",
		"rayservices",
	) {
		t.Fatal("expected false when RESTMapper is nil")
	}

	emptyMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "ray.io", Version: "v1"}})
	if IsAPIResourceInstalled(
		restMapperClient{Client: baseClient, mapper: emptyMapper},
		nil,
		"ray.io",
		"v1",
		"RayService",
		"rayservices",
	) {
		t.Fatal("expected false when RESTMapper has no matching mapping")
	}
}

type providerConfigStatusTestCase struct {
	name               string
	ready              bool
	version            string
	upstreamCRDVersion string
}

func TestUpdateProviderConfigStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}

	testCases := []providerConfigStatusTestCase{
		{
			name:               "dynamo",
			ready:              true,
			version:            "dynamo-provider:test",
			upstreamCRDVersion: "nvidia.com/v1alpha1",
		},
		{
			name:               "kuberay",
			ready:              false,
			version:            "kuberay-provider:test",
			upstreamCRDVersion: "ray.io/v1",
		},
		{
			name:               "vllm",
			ready:              true,
			version:            "vllm-provider:test",
			upstreamCRDVersion: "apps/v1",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testProviderConfigStatusUpdate(t, scheme, tc)
		})
	}
}

func testProviderConfigStatusUpdate(t *testing.T, scheme *runtime.Scheme, tc providerConfigStatusTestCase) {
	t.Helper()
	existing := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: tc.name},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			SelectionRules: []airunwayv1alpha1.SelectionRule{{Condition: "true", Priority: 1}},
		},
	}

	testClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existing).
		WithStatusSubresource(existing).
		Build()

	err := UpdateProviderConfigStatus(
		context.Background(),
		testClient,
		tc.name,
		tc.ready,
		tc.version,
		tc.upstreamCRDVersion,
	)
	if err != nil {
		t.Fatalf("UpdateProviderConfigStatus() unexpected error: %v", err)
	}

	var updated airunwayv1alpha1.InferenceProviderConfig
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: tc.name}, &updated); err != nil {
		t.Fatalf("failed to fetch updated config: %v", err)
	}

	if updated.Status.Ready != tc.ready {
		t.Fatalf("status.ready = %v, want %v", updated.Status.Ready, tc.ready)
	}
	if updated.Status.Version != tc.version {
		t.Fatalf("status.version = %q, want %q", updated.Status.Version, tc.version)
	}
	if updated.Status.UpstreamCRDVersion != tc.upstreamCRDVersion {
		t.Fatalf("status.upstreamCRDVersion = %q, want %q", updated.Status.UpstreamCRDVersion, tc.upstreamCRDVersion)
	}
	if updated.Status.LastHeartbeat == nil {
		t.Fatal("expected status.lastHeartbeat to be set")
	}

	if len(updated.Spec.SelectionRules) != 1 || updated.Spec.SelectionRules[0].Priority != 1 {
		t.Fatalf("spec changed unexpectedly: %#v", updated.Spec.SelectionRules)
	}
}

func TestUpdateProviderConfigStatusReturnsGetError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}

	testClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(
			ctx context.Context,
			cl client.WithWatch,
			key client.ObjectKey,
			obj client.Object,
			opts ...client.GetOption,
		) error {
			return errors.New("boom")
		},
	}).Build()

	err := UpdateProviderConfigStatus(
		context.Background(),
		testClient,
		"broken-provider",
		true,
		"broken-provider:test",
		"apps/v1",
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to get InferenceProviderConfig") {
		t.Fatalf("error = %q, want substring %q", err.Error(), "failed to get InferenceProviderConfig")
	}
}

func TestUpdateProviderConfigStatusReturnsStatusUpdateError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}

	existing := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "provider"},
	}

	testClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existing).
		WithStatusSubresource(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				ctx context.Context,
				cl client.Client,
				subResourceName string,
				obj client.Object,
				opts ...client.SubResourceUpdateOption,
			) error {
				if subResourceName == "status" {
					return errors.New("status update failed")
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()

	err := UpdateProviderConfigStatus(
		context.Background(),
		testClient,
		"provider",
		true,
		"provider:test",
		"apps/v1",
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to update InferenceProviderConfig status") {
		t.Fatalf("error = %q, want substring %q", err.Error(), "failed to update InferenceProviderConfig status")
	}
}
