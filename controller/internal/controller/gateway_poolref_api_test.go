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

package controller

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/internal/gateway"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// These tests enter Reconcile with real API discovery, schema/defaulting,
// generation, status subresources, resource versions, and deletion preconditions.
// Only provider-reported Running status is seeded; no inference workload runs.
func TestGatewayPoolRefAPI(t *testing.T) {
	config, apiClient, scheme := startGatewayPoolRefAPI(t)
	environment := &gatewayPoolRefAPIEnv{config: config, client: apiClient, scheme: scheme}
	t.Run("initial user-owned labels and stable no-op", environment.testInitialUserMode)
	t.Run("only current generation rejects the pool", environment.testGenerations)
	for _, mode := range []string{"enabled", "disabled", "missing gateway", "disabled without gateway"} {
		t.Run("managed transition/"+mode, func(t *testing.T) { environment.testManagedTransition(t, mode) })
	}
	for _, failure := range []string{"EPP delete", "EPP delete while disabled", "EPP delete during deletion", gatewayPodConflict, "pool replacement"} {
		t.Run("partial cleanup retries/"+failure, func(t *testing.T) { environment.testCleanupRetry(t, failure) })
	}
	t.Run("disabled user mode preserves foreign generated names", environment.testForeignNames)
	for _, ownership := range []string{"unowned", "other controller"} {
		t.Run("enabled route collision/"+ownership, func(t *testing.T) { environment.testEnabledRouteCollision(t, ownership) })
	}
	for _, mode := range []string{"controller managed", gatewayFinalLabelAttempt, "provider managed"} {
		t.Run("managed pod conflict/"+mode, func(t *testing.T) { environment.testManagedPodConflict(t, mode) })
	}
	t.Run("pool watch propagates creation status and deletion", environment.testWatch)
}

const gatewayPodConflict = "pod conflict"

type gatewayPoolRefAPIEnv struct {
	config *rest.Config
	client client.WithWatch
	scheme *runtime.Scheme
}

type gatewayPoolConditionCase struct {
	name       string
	generation int64
	status     metav1.ConditionStatus
	parent     string
	want       metav1.ConditionStatus
}

func (e *gatewayPoolRefAPIEnv) testInitialUserMode(t *testing.T) {
	config, apiClient, scheme := e.config, e.client, e.scheme
	f := newGatewayPoolRefAPIFixture(t, apiClient)
	f.usePool(t, false)
	unchanged := f.snapshot(t, f.pool, f.epp, f.eppService, f.pod, f.userPod)
	r := newGatewayAPIReconciler(t, config, apiClient, scheme, f.md.Namespace)
	f.reconcile(t, r)
	f.assertLabel(t, f.userPod, f.md.Name)
	f.assertLabel(t, f.pod, "")
	f.assertUnchanged(t, unchanged)
	f.assertRoute(t)
	unchanged = append(unchanged, f.snapshot(t, &gatewayv1.HTTPRoute{ObjectMeta: f.md.ObjectMeta})...)
	for range 3 {
		f.reconcile(t, r)
	}
	f.assertUnchanged(t, unchanged)
}

func (e *gatewayPoolRefAPIEnv) testGenerations(t *testing.T) {
	config, apiClient, scheme := e.config, e.client, e.scheme
	f := newGatewayPoolRefAPIFixture(t, apiClient)
	f.usePool(t, false)
	if f.pool.Generation != 1 {
		t.Fatalf("API-created pool generation = %d, want 1", f.pool.Generation)
	}
	f.pool.Spec.TargetPorts[0].Number++
	if err := apiClient.Update(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	if f.pool.Generation != 2 {
		t.Fatalf("API-updated pool generation = %d, want 2", f.pool.Generation)
	}
	r := newGatewayAPIReconciler(t, config, apiClient, scheme, f.md.Namespace)
	for _, conditionType := range []string{"Accepted", "ResolvedRefs"} {
		for _, tc := range []gatewayPoolConditionCase{
			{name: "omitted", generation: 0, status: metav1.ConditionFalse, parent: "gateway", want: metav1.ConditionTrue},
			{name: "stale", generation: 1, status: metav1.ConditionFalse, parent: "gateway", want: metav1.ConditionTrue},
			{name: "future", generation: 3, status: metav1.ConditionFalse, parent: "gateway", want: metav1.ConditionTrue},
			{name: "current", generation: 2, status: metav1.ConditionFalse, parent: "gateway", want: metav1.ConditionFalse},
			{name: "unknown", generation: 2, status: metav1.ConditionUnknown, parent: "gateway", want: metav1.ConditionTrue},
			{name: "accepted", generation: 2, status: metav1.ConditionTrue, parent: "gateway", want: metav1.ConditionTrue},
			{name: "other parent", generation: 2, status: metav1.ConditionFalse, parent: "other-gateway", want: metav1.ConditionTrue},
		} {
			t.Run(conditionType+"/"+tc.name, func(t *testing.T) {
				f.assertPoolCondition(t, r, conditionType, tc)
			})
		}
	}
}

func (e *gatewayPoolRefAPIEnv) testManagedTransition(t *testing.T, mode string) {
	config, apiClient, scheme := e.config, e.client, e.scheme
	f := newGatewayPoolRefAPIFixture(t, apiClient)
	r := newGatewayAPIReconciler(t, config, apiClient, scheme, f.md.Namespace)
	unchanged := f.snapshot(t, f.pool, f.epp, f.eppService, f.userPod)
	// Obtain provenance from the real service-selection and pod-patching
	// path. A fixture-applied ownership marker would not prove this hop.
	f.reconcile(t, r)
	f.assertLabel(t, f.pod, f.md.Name)
	for _, obj := range f.managedResources() {
		f.get(t, client.ObjectKeyFromObject(obj), obj)
		if !metav1.IsControlledBy(obj, f.md) {
			t.Fatalf("managed %T is not controlled by the real ModelDeployment UID", obj)
		}
	}
	disabled := strings.HasPrefix(mode, "disabled")
	f.usePool(t, disabled)
	// Recreate the reconciler so provenance must come from API state.
	r = newGatewayAPIReconciler(t, config, apiClient, scheme, f.md.Namespace)
	if strings.Contains(mode, "gateway") {
		f.removeGateway(t)
		r.GatewayDetector.ExplicitGatewayName = ""
		r.GatewayDetector.ExplicitGatewayNamespace = ""
	}
	f.reconcile(t, r)
	f.assertManagedResourcesGone(t)
	f.assertLabel(t, f.pod, "")
	f.assertLabel(t, f.userPod, f.md.Name)
	f.assertUnchanged(t, unchanged)
	if mode == "enabled" {
		f.assertRoute(t)
	}
	if disabled {
		var route gatewayv1.HTTPRoute
		if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(f.md), &route); !apierrors.IsNotFound(err) {
			t.Errorf("disabled managed route remains: %v", err)
		}
	}
	unchanged = append(unchanged, f.snapshot(t, f.pod)...)
	f.reconcile(t, r)
	f.assertUnchanged(t, unchanged)
}

func (e *gatewayPoolRefAPIEnv) testCleanupRetry(t *testing.T, failure string) {
	config, apiClient, scheme := e.config, e.client, e.scheme
	f := newGatewayPoolRefAPIFixture(t, apiClient)
	r := newGatewayAPIReconciler(t, config, apiClient, scheme, f.md.Namespace)
	f.reconcile(t, r)
	disabled := strings.Contains(failure, "disabled")
	deleting := strings.Contains(failure, "during deletion")
	f.usePool(t, disabled)
	if deleting {
		f.md.Finalizers = []string{"tests.airunway.ai/hold"}
		if err := apiClient.Update(t.Context(), f.md); err != nil {
			t.Fatal(err)
		}
		if err := apiClient.Delete(t.Context(), f.md); err != nil {
			t.Fatal(err)
		}
	}
	fault := &gatewayPoolRefAPIFault{fixture: f, failure: failure}
	r.Client = interceptor.NewClient(apiClient, interceptor.Funcs{Delete: fault.delete, Patch: fault.patch})
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.md)})
	if !fault.injected {
		t.Fatal("intended failure boundary was not reached")
	}
	if err == nil {
		t.Error("cleanup failure was swallowed instead of requesting a controller retry")
	}
	if failure == gatewayPodConflict {
		f.assertLabel(t, f.pod, "external-owner")
	}
	if failure == "pool replacement" {
		foreign := &inferencev1.InferencePool{}
		f.get(t, client.ObjectKeyFromObject(f.md), foreign)
		if len(foreign.OwnerReferences) != 0 {
			t.Error("replacement pool was adopted")
		}
	}
	// A fresh reconciliation must finish partial work using only API state.
	r = newGatewayAPIReconciler(t, config, apiClient, scheme, f.md.Namespace)
	f.reconcile(t, r)
	wantLabel := ""
	if failure == gatewayPodConflict {
		wantLabel = "external-owner"
	}
	f.assertLabel(t, f.pod, wantLabel)
	f.assertLabel(t, f.userPod, f.md.Name)
	if !disabled && !deleting {
		f.assertRoute(t)
	} else {
		var route gatewayv1.HTTPRoute
		if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(f.md), &route); !apierrors.IsNotFound(err) {
			t.Errorf("managed route remains after cleanup retry: %v", err)
		}
	}
}

func (e *gatewayPoolRefAPIEnv) testForeignNames(t *testing.T) {
	config, apiClient, scheme := e.config, e.client, e.scheme
	f := newGatewayPoolRefAPIFixture(t, apiClient)
	f.usePool(t, true)
	foreignPool := f.pool.DeepCopy()
	foreignPool.ObjectMeta = metav1.ObjectMeta{Name: f.md.Name, Namespace: f.md.Namespace}
	foreignEPP := f.epp.DeepCopy()
	foreignEPP.ObjectMeta = metav1.ObjectMeta{Name: f.md.Name + "-epp", Namespace: f.md.Namespace}
	port := gatewayv1.PortNumber(8080)
	foreignRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: f.md.Name, Namespace: f.md.Namespace},
		Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{{
			BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "user-service", Port: &port},
			}}},
		}}},
	}
	for _, obj := range []client.Object{foreignPool, foreignEPP, foreignRoute} {
		if err := apiClient.Create(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}
	unchanged := f.snapshot(t, foreignPool, foreignEPP, foreignRoute, f.pool, f.epp, f.eppService, f.userPod)
	f.reconcile(t, newGatewayAPIReconciler(t, config, apiClient, scheme, f.md.Namespace))
	f.assertUnchanged(t, unchanged)
}

func (e *gatewayPoolRefAPIEnv) testWatch(t *testing.T) {
	apiClient := e.client
	f := newGatewayPoolRefAPIFixture(t, apiClient)
	f.md.Spec.Gateway.PoolRef = "watched-pool"
	if err := apiClient.Update(t.Context(), f.md); err != nil {
		t.Fatal(err)
	}
	e.startManager(t, f.md.Namespace)
	f.waitGatewayCondition(t, metav1.ConditionFalse, "InferencePoolNotFound")
	// From here, only pool events can drive the desired changes. The test
	// neither calls Reconcile nor modifies the ModelDeployment again.
	pool := f.pool.DeepCopy()
	pool.ObjectMeta = metav1.ObjectMeta{Name: "watched-pool", Namespace: f.md.Namespace}
	if err := apiClient.Create(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	f.waitGatewayCondition(t, metav1.ConditionTrue, "GatewayConfigured")
	f.pool = pool
	f.assertRoute(t)
	pool.Status.Parents = []inferencev1.ParentStatus{{
		ParentRef: inferencev1.ParentReference{Name: "gateway"},
		Conditions: []metav1.Condition{{
			Type: "Accepted", Status: metav1.ConditionFalse, ObservedGeneration: pool.Generation,
			Reason: "PoolStatus", LastTransitionTime: metav1.Now(),
		}},
	}}
	if err := apiClient.Status().Update(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	f.waitGatewayCondition(t, metav1.ConditionFalse, "InferencePoolNotReady")
	pool.Status.Parents[0].Conditions[0].ObservedGeneration = 0
	if err := apiClient.Status().Update(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	f.waitGatewayCondition(t, metav1.ConditionTrue, "GatewayConfigured")
	if err := apiClient.Delete(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	f.waitGatewayCondition(t, metav1.ConditionFalse, "InferencePoolNotFound")
	var route gatewayv1.HTTPRoute
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(f.md), &route); !apierrors.IsNotFound(err) {
		t.Errorf("missing pool left a stale route: %v", err)
	}
	f.assertLabel(t, f.userPod, f.md.Name)
}

func (f *gatewayPoolRefAPIFixture) assertPoolCondition(t *testing.T, r *ModelDeploymentReconciler, conditionType string, tc gatewayPoolConditionCase) {
	t.Helper()
	f.pool.Status.Parents = []inferencev1.ParentStatus{{
		ParentRef: inferencev1.ParentReference{Name: inferencev1.ObjectName(tc.parent)},
		Conditions: []metav1.Condition{{
			Type: conditionType, Status: tc.status, ObservedGeneration: tc.generation,
			Reason: "PoolStatus", Message: "endpoint picker status", LastTransitionTime: metav1.Now(),
		}},
	}}
	if err := f.client.Status().Update(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, r)
	md := &airunwayv1alpha1.ModelDeployment{}
	f.get(t, client.ObjectKeyFromObject(f.md), md)
	condition := meta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeGatewayReady)
	if condition == nil {
		t.Fatal("persisted GatewayReady condition is missing")
	}
	if condition.Status != tc.want {
		t.Errorf("persisted GatewayReady = %#v, want %s", condition, tc.want)
	}
	if tc.want == metav1.ConditionFalse && (condition.Reason != "InferencePoolNotReady" || !strings.Contains(condition.Message, conditionType+"=False")) {
		t.Errorf("missing current rejection details: %#v", condition)
	}
	f.assertRoute(t)
}

func (e *gatewayPoolRefAPIEnv) startManager(t *testing.T, namespace string) {
	t.Helper()
	config, scheme := e.config, e.scheme
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newGatewayAPIReconciler(t, config, mgr.GetClient(), scheme, namespace)
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	managerContext, stop := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(managerContext) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-stopped:
			if err != nil {
				t.Errorf("manager stopped with error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})
}

type gatewayPoolRefAPIFault struct {
	fixture  *gatewayPoolRefAPIFixture
	failure  string
	injected bool
}

func (fault *gatewayPoolRefAPIFault) delete(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
	f := fault.fixture
	if !fault.injected && strings.HasPrefix(fault.failure, "EPP delete") && obj.GetName() == f.md.Name+"-epp" {
		fault.injected = true
		return apierrors.NewServiceUnavailable("injected EPP deletion failure")
	}
	if _, isPool := obj.(*inferencev1.InferencePool); !fault.injected && fault.failure == "pool replacement" && isPool && obj.GetName() == f.md.Name {
		fault.injected = true
		if err := f.client.Delete(ctx, obj); err != nil {
			return err
		}
		replacement := f.pool.DeepCopy()
		replacement.ObjectMeta = metav1.ObjectMeta{Name: f.md.Name, Namespace: f.md.Namespace}
		if err := f.client.Create(ctx, replacement); err != nil {
			return err
		}
	}
	return c.Delete(ctx, obj, opts...)
}

func (fault *gatewayPoolRefAPIFault) patch(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	f := fault.fixture
	if _, isPod := obj.(*corev1.Pod); !fault.injected && fault.failure == gatewayPodConflict && isPod && obj.GetName() == f.pod.Name {
		fault.injected = true
		current := &corev1.Pod{}
		if err := f.client.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return err
		}
		current.Labels[airunwayv1alpha1.LabelModelDeployment] = "external-owner"
		if err := f.client.Update(ctx, current); err != nil {
			return err
		}
	}
	return c.Patch(ctx, obj, patch, opts...)
}

func startGatewayPoolRefAPI(t *testing.T) (*rest.Config, client.WithWatch, *runtime.Scheme) {
	t.Helper()
	paths := []string{
		filepath.Join("..", "..", "config", "crd", "bases", "airunway.ai_modeldeployments.yaml"),
		filepath.Join("..", "..", "config", "crd", "bases", "airunway.ai_inferenceproviderconfigs.yaml"),
	}
	for _, dependency := range []struct{ module, directory string }{
		{"sigs.k8s.io/gateway-api", "config/crd/standard"},
		{"sigs.k8s.io/gateway-api-inference-extension", "config/crd/bases"},
	} {
		// Resolve the version from this module's go.mod, rather than keeping
		// copied schemas or a second hardcoded version in the test fixture.
		commandContext, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		output, err := exec.CommandContext(commandContext, "go", "list", "-m", "-f", "{{.Dir}}", dependency.module).Output()
		cancel()
		if err != nil {
			t.Fatalf("resolve pinned %s CRDs: %v", dependency.module, err)
		}
		paths = append(paths, filepath.Join(strings.TrimSpace(string(output)), dependency.directory))
	}
	apiEnv := &envtest.Environment{
		CRDDirectoryPaths: paths, ErrorIfCRDPathMissing: true,
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: time.Minute,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	config, err := apiEnv.Start()
	if err != nil {
		t.Fatalf("start gateway API environment: %v", err)
	}
	t.Cleanup(func() {
		if err := apiEnv.Stop(); err != nil {
			t.Errorf("stop gateway API environment: %v", err)
		}
	})
	scheme := newTestScheme()
	apiClient, err := client.NewWithWatch(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return config, apiClient, scheme
}

type gatewayPoolRefAPIFixture struct {
	client       client.WithWatch
	md           *airunwayv1alpha1.ModelDeployment
	pool         *inferencev1.InferencePool
	epp          *appsv1.Deployment
	eppService   *corev1.Service
	pod, userPod *corev1.Pod
	gateway      *gatewayv1.Gateway
}

func newGatewayPoolRefAPIFixture(t *testing.T, c client.WithWatch) *gatewayPoolRefAPIFixture {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "gateway-poolref-"}}
	if err := c.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	f := &gatewayPoolRefAPIFixture{client: c}
	f.md = &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: ns.Name},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model:     airunwayv1alpha1.ModelSpec{ID: "test/model"},
			Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Provider:  &airunwayv1alpha1.ProviderSpec{Name: "vllm"},
			Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}},
			Gateway:   &airunwayv1alpha1.GatewaySpec{ModelName: "test-model"},
		},
	}
	if err := c.Create(t.Context(), f.md); err != nil {
		t.Fatal(err)
	}
	f.md.Status = airunwayv1alpha1.ModelDeploymentStatus{
		Phase:    airunwayv1alpha1.DeploymentPhaseRunning,
		Provider: &airunwayv1alpha1.ProviderStatus{Name: "vllm"},
		Endpoint: &airunwayv1alpha1.EndpointStatus{Service: "model-service", Port: 8000},
	}
	if err := c.Status().Update(t.Context(), f.md); err != nil {
		t.Fatal(err)
	}
	f.pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "model-pod", Namespace: ns.Name, Labels: map[string]string{"app": "model", "unrelated": "keep"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "registry.k8s.io/pause:3.10"}}},
	}
	f.userPod = f.pod.DeepCopy()
	f.userPod.Name = "user-labelled-pod"
	f.userPod.Labels[airunwayv1alpha1.LabelModelDeployment] = f.md.Name
	f.eppService = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-epp", Namespace: ns.Name},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "grpc", Port: 9002}}},
	}
	f.epp = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-epp", Namespace: ns.Name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "shared-epp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "shared-epp"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "epp", Image: "registry.k8s.io/pause:3.10"}}},
			},
		},
	}
	f.pool = &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-pool", Namespace: ns.Name},
		Spec: inferencev1.InferencePoolSpec{
			Selector:          inferencev1.LabelSelector{MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{inferencev1.LabelKey(airunwayv1alpha1.LabelModelDeployment): inferencev1.LabelValue(f.md.Name)}},
			TargetPorts:       []inferencev1.Port{{Number: 8000}},
			EndpointPickerRef: inferencev1.EndpointPickerRef{Name: "shared-epp", Port: &inferencev1.Port{Number: 9002}},
		},
	}
	f.gateway = &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: ns.Name},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "test", Listeners: []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}}},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "model-service", Namespace: ns.Name},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "model"}, Ports: []corev1.ServicePort{{Name: "http", Port: 8000}}},
	}
	for _, obj := range []client.Object{f.pod, f.userPod, f.epp, f.eppService, f.pool, f.gateway, service} {
		if err := c.Create(t.Context(), obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		if err := client.IgnoreNotFound(c.Delete(context.Background(), f.gateway)); err != nil {
			t.Errorf("remove fixture gateway: %v", err)
		}
	})
	return f
}

func newGatewayAPIReconciler(t *testing.T, config *rest.Config, c client.Client, scheme *runtime.Scheme, namespace string) *ModelDeploymentReconciler {
	t.Helper()
	dc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	detector := gateway.NewDetector(dc)
	detector.ExplicitGatewayName, detector.ExplicitGatewayNamespace = "gateway", namespace
	if !detector.IsAvailable(t.Context()) {
		t.Fatal("production discovery did not find Gateway API and InferencePool CRDs")
	}
	return &ModelDeploymentReconciler{Client: c, Scheme: scheme, GatewayDetector: detector, ProviderResolver: gateway.NewInferenceProviderConfigResolver(c)}
}

func (f *gatewayPoolRefAPIFixture) get(t *testing.T, key client.ObjectKey, obj client.Object) {
	t.Helper()
	if err := f.client.Get(t.Context(), key, obj); err != nil {
		t.Fatalf("get %T %s: %v", obj, key, err)
	}
}

func (f *gatewayPoolRefAPIFixture) usePool(t *testing.T, disabled bool) {
	t.Helper()
	f.get(t, client.ObjectKeyFromObject(f.md), f.md)
	f.md.Spec.Gateway.PoolRef = f.pool.Name
	f.md.Spec.Gateway.Enabled = boolPtr(!disabled)
	if err := f.client.Update(t.Context(), f.md); err != nil {
		t.Fatal(err)
	}
}

func (f *gatewayPoolRefAPIFixture) removeGateway(t *testing.T) {
	t.Helper()
	if err := f.client.Delete(t.Context(), f.gateway); err != nil {
		t.Fatal(err)
	}
}

func (f *gatewayPoolRefAPIFixture) reconcile(t *testing.T, r *ModelDeploymentReconciler) {
	t.Helper()
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.md)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func (f *gatewayPoolRefAPIFixture) assertLabel(t *testing.T, pod *corev1.Pod, want string) {
	t.Helper()
	current := &corev1.Pod{}
	f.get(t, client.ObjectKeyFromObject(pod), current)
	if got := current.Labels[airunwayv1alpha1.LabelModelDeployment]; got != want {
		t.Errorf("pod %s selector label = %q, want %q", pod.Name, got, want)
	}
	if current.Labels["unrelated"] != "keep" {
		t.Error("unrelated pod label changed")
	}
}

func (f *gatewayPoolRefAPIFixture) assertRoute(t *testing.T) {
	t.Helper()
	route := &gatewayv1.HTTPRoute{}
	f.get(t, client.ObjectKeyFromObject(f.md), route)
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 {
		t.Fatalf("unexpected HTTPRoute rules: %#v", route.Spec.Rules)
	}
	backend := route.Spec.Rules[0].BackendRefs[0]
	if string(backend.Name) != f.pool.Name || backend.Kind == nil || *backend.Kind != "InferencePool" || backend.Namespace == nil || string(*backend.Namespace) != f.md.Namespace {
		t.Errorf("route does not target the referenced pool: %#v", backend)
	}
}

func (f *gatewayPoolRefAPIFixture) snapshot(t *testing.T, objects ...client.Object) []client.Object {
	t.Helper()
	snapshots := make([]client.Object, 0, len(objects))
	for _, obj := range objects {
		copy := obj.DeepCopyObject().(client.Object)
		f.get(t, client.ObjectKeyFromObject(obj), copy)
		snapshots = append(snapshots, copy)
	}
	return snapshots
}

func (f *gatewayPoolRefAPIFixture) assertUnchanged(t *testing.T, snapshots []client.Object) {
	t.Helper()
	for _, before := range snapshots {
		after := before.DeepCopyObject().(client.Object)
		f.get(t, client.ObjectKeyFromObject(before), after)
		if before.GetResourceVersion() != after.GetResourceVersion() {
			t.Errorf("unexpected write to %T %s: resourceVersion %s -> %s", before, before.GetName(), before.GetResourceVersion(), after.GetResourceVersion())
		}
	}
}

func (f *gatewayPoolRefAPIFixture) managedResources() []client.Object {
	poolMeta := metav1.ObjectMeta{Name: f.md.Name, Namespace: f.md.Namespace}
	eppMeta := metav1.ObjectMeta{Name: f.md.Name + "-epp", Namespace: f.md.Namespace}
	return []client.Object{
		&inferencev1.InferencePool{ObjectMeta: poolMeta},
		&appsv1.Deployment{ObjectMeta: eppMeta}, &corev1.Service{ObjectMeta: eppMeta},
		&corev1.ConfigMap{ObjectMeta: eppMeta}, &corev1.ServiceAccount{ObjectMeta: eppMeta},
		&rbacv1.Role{ObjectMeta: eppMeta}, &rbacv1.RoleBinding{ObjectMeta: eppMeta},
	}
}

func (f *gatewayPoolRefAPIFixture) assertManagedResourcesGone(t *testing.T) {
	t.Helper()
	for _, obj := range f.managedResources() {
		if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Errorf("managed %T remains after transition: %v", obj, err)
		}
	}
}

func (f *gatewayPoolRefAPIFixture) waitGatewayCondition(t *testing.T, status metav1.ConditionStatus, reason string) {
	t.Helper()
	var last *metav1.Condition
	err := wait.PollUntilContextTimeout(t.Context(), 50*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		md := &airunwayv1alpha1.ModelDeployment{}
		if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.md), md); err != nil {
			return false, err
		}
		last = meta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeGatewayReady)
		return last != nil && last.Status == status && last.Reason == reason, nil
	})
	if err != nil {
		t.Fatalf("wait for watch-driven GatewayReady=%s/%s: %v; last=%#v", status, reason, err, last)
	}
}
