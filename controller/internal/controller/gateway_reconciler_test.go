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
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"github.com/kaito-project/airunway/controller/internal/gateway"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(airunwayv1alpha1.AddToScheme(s))
	utilruntime.Must(gatewayv1.Install(s))
	utilruntime.Must(gatewayv1beta1.Install(s))
	utilruntime.Must(inferencev1.Install(s))
	return s
}

func boolPtr(b bool) *bool { return &b }

// newTestReconciler creates a ModelDeploymentReconciler with a fake client and
// an optional gateway detector.
func newTestReconciler(scheme *runtime.Scheme, detector *gateway.Detector, objs ...client.Object) *ModelDeploymentReconciler {
	cb := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&airunwayv1alpha1.ModelDeployment{})
	if len(objs) > 0 {
		cb = cb.WithObjects(objs...)
	}
	return &ModelDeploymentReconciler{
		Client:          cb.Build(),
		Scheme:          scheme,
		GatewayDetector: detector,
	}
}

func newModelDeployment(name, ns string) *airunwayv1alpha1.ModelDeployment {
	return &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       types.UID(ns + "/" + name),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID:     "meta-llama/Llama-3-8B",
				Source: airunwayv1alpha1.ModelSourceHuggingFace,
			},
		},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Phase: airunwayv1alpha1.DeploymentPhaseRunning,
			Endpoint: &airunwayv1alpha1.EndpointStatus{
				Service: "test-model-svc",
				Port:    8080,
			},
		},
	}
}

// fakeDetector returns a Detector with explicit gateway config and availability set.
func fakeDetector(available bool, gwName, gwNs string) *gateway.Detector {
	dc := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	if available {
		dc.Resources = []*metav1.APIResourceList{
			{
				GroupVersion: "inference.networking.k8s.io/v1",
				APIResources: []metav1.APIResource{{Name: "inferencepools"}},
			},
			{
				GroupVersion: "gateway.networking.k8s.io/v1",
				APIResources: []metav1.APIResource{{Name: "httproutes"}, {Name: "gateways"}},
			},
		}
	}
	d := gateway.NewDetector(dc)
	d.ExplicitGatewayName = gwName
	d.ExplicitGatewayNamespace = gwNs
	// Warm the cache
	d.IsAvailable(context.Background())
	return d
}

// newTestGateway creates a minimal Gateway object in the given namespace.
func newTestGateway(name, ns string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "istio"},
	}
}

// --- Tests ---

func TestGateway_InferencePoolCreation(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	err := r.reconcileInferencePool(ctx, md, 8080, "gateway-ns")
	if err != nil {
		t.Fatalf("reconcileInferencePool failed: %v", err)
	}

	// Verify InferencePool was created
	var pool inferencev1.InferencePool
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &pool); err != nil {
		t.Fatalf("InferencePool not found: %v", err)
	}

	// Check selector labels
	expectedLabel := inferencev1.LabelKey(airunwayv1alpha1.LabelModelDeployment)
	val, ok := pool.Spec.Selector.MatchLabels[expectedLabel]
	if !ok {
		t.Errorf("expected selector label %s not found", expectedLabel)
	}
	if string(val) != "test-model" {
		t.Errorf("expected selector label value %q, got %q", "test-model", val)
	}

	// Check target port
	if len(pool.Spec.TargetPorts) != 1 {
		t.Fatalf("expected 1 target port, got %d", len(pool.Spec.TargetPorts))
	}
	if pool.Spec.TargetPorts[0].Number != 8080 {
		t.Errorf("expected target port 8080, got %d", pool.Spec.TargetPorts[0].Number)
	}

	// Check EndpointPickerRef
	if string(pool.Spec.EndpointPickerRef.Name) != "test-model-epp" {
		t.Errorf("expected EndpointPickerRef name %q, got %q", "test-model-epp", pool.Spec.EndpointPickerRef.Name)
	}
	if pool.Spec.EndpointPickerRef.Port == nil || pool.Spec.EndpointPickerRef.Port.Number != 9002 {
		t.Errorf("expected EndpointPickerRef port 9002, got %v", pool.Spec.EndpointPickerRef.Port)
	}

	// Check OwnerReference
	if len(pool.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(pool.OwnerReferences))
	}
	if pool.OwnerReferences[0].Name != "test-model" {
		t.Errorf("expected owner ref name %q, got %q", "test-model", pool.OwnerReferences[0].Name)
	}
	if pool.OwnerReferences[0].Kind != "ModelDeployment" {
		t.Errorf("expected owner ref kind %q, got %q", "ModelDeployment", pool.OwnerReferences[0].Kind)
	}
}

func TestGateway_InferencePoolDefaultPort(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Status.Endpoint = nil // no endpoint, should use default port
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	// reconcileGateway uses default port 8000 when no endpoint
	err := r.reconcileInferencePool(ctx, md, 8000, "gateway-ns")
	if err != nil {
		t.Fatalf("reconcileInferencePool failed: %v", err)
	}

	var pool inferencev1.InferencePool
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &pool); err != nil {
		t.Fatalf("InferencePool not found: %v", err)
	}
	if pool.Spec.TargetPorts[0].Number != 8000 {
		t.Errorf("expected default target port 8000, got %d", pool.Spec.TargetPorts[0].Number)
	}
}

func TestGateway_HTTPRouteCreation(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	gwConfig := &gateway.GatewayConfig{
		GatewayName:      "my-gateway",
		GatewayNamespace: "gateway-ns",
	}

	err := r.reconcileHTTPRoute(ctx, md, gwConfig, "meta-llama/Llama-3-8B", httpRouteBackendTarget{
		group:     "inference.networking.k8s.io",
		kind:      "InferencePool",
		name:      md.Name,
		namespace: md.Namespace,
	})
	if err != nil {
		t.Fatalf("reconcileHTTPRoute failed: %v", err)
	}

	// Verify HTTPRoute was created
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &route); err != nil {
		t.Fatalf("HTTPRoute not found: %v", err)
	}

	// Check parent ref points to the gateway
	if len(route.Spec.ParentRefs) != 1 {
		t.Fatalf("expected 1 parent ref, got %d", len(route.Spec.ParentRefs))
	}
	parentRef := route.Spec.ParentRefs[0]
	if string(parentRef.Name) != "my-gateway" {
		t.Errorf("expected parent ref name %q, got %q", "my-gateway", parentRef.Name)
	}
	if parentRef.Namespace == nil || string(*parentRef.Namespace) != "gateway-ns" {
		t.Errorf("expected parent ref namespace %q, got %v", "gateway-ns", parentRef.Namespace)
	}

	// Check backend ref points to InferencePool
	if len(route.Spec.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(route.Spec.Rules))
	}
	if len(route.Spec.Rules[0].BackendRefs) != 1 {
		t.Fatalf("expected 1 backend ref, got %d", len(route.Spec.Rules[0].BackendRefs))
	}
	backendRef := route.Spec.Rules[0].BackendRefs[0]
	if string(backendRef.Name) != "test-model" {
		t.Errorf("expected backend ref name %q, got %q", "test-model", backendRef.Name)
	}
	if backendRef.Group == nil || string(*backendRef.Group) != "inference.networking.k8s.io" {
		t.Errorf("expected backend ref group %q, got %v", "inference.networking.k8s.io", backendRef.Group)
	}
	if backendRef.Kind == nil || string(*backendRef.Kind) != "InferencePool" {
		t.Errorf("expected backend ref kind %q, got %v", "InferencePool", backendRef.Kind)
	}

	// Check OwnerReference
	if len(route.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(route.OwnerReferences))
	}
	if route.OwnerReferences[0].Name != "test-model" {
		t.Errorf("expected owner ref name %q, got %q", "test-model", route.OwnerReferences[0].Name)
	}
}

func TestGateway_DisabledSkipsCreation(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{
		Enabled: boolPtr(false),
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	err := r.reconcileGateway(ctx, md)
	if err != nil {
		t.Fatalf("reconcileGateway failed: %v", err)
	}

	// Verify no InferencePool was created
	var pool inferencev1.InferencePool
	err = r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &pool)
	if err == nil {
		t.Error("expected InferencePool to NOT be created when gateway is disabled")
	}

	// Verify no HTTPRoute was created
	var route gatewayv1.HTTPRoute
	err = r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &route)
	if err == nil {
		t.Error("expected HTTPRoute to NOT be created when gateway is disabled")
	}
}

func TestGateway_DisabledCleansUpExistingResources(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	detector := fakeDetector(true, "my-gateway", "gateway-ns")

	// Pre-create gateway resources
	pool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	r := newTestReconciler(scheme, detector, md, pool, route)
	ctx := context.Background()

	err := r.cleanupGatewayResources(ctx, md)
	if err != nil {
		t.Fatalf("cleanupGatewayResources failed: %v", err)
	}

	// Verify InferencePool was deleted
	var p inferencev1.InferencePool
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &p); err == nil {
		t.Error("expected InferencePool to be deleted")
	}

	// Verify HTTPRoute was deleted
	var rt gatewayv1.HTTPRoute
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &rt); err == nil {
		t.Error("expected HTTPRoute to be deleted")
	}

	// Verify gateway status is cleared
	if md.Status.Gateway != nil {
		t.Error("expected gateway status to be nil after cleanup")
	}

	// Verify GatewayReady condition is set to False
	found := false
	for _, c := range md.Status.Conditions {
		if c.Type == airunwayv1alpha1.ConditionTypeGatewayReady {
			found = true
			if c.Status != metav1.ConditionFalse {
				t.Errorf("expected GatewayReady condition to be False after cleanup, got %s", c.Status)
			}
			if c.Reason != "GatewayDisabled" {
				t.Errorf("expected reason GatewayDisabled, got %s", c.Reason)
			}
		}
	}
	if !found {
		t.Error("expected GatewayReady condition to be set after cleanup")
	}
}

func TestGateway_CleanupOnPhaseTransition(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	// Simulate a deployment that was Running with gateway resources
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
	md.Status.Gateway = &airunwayv1alpha1.GatewayStatus{
		Endpoint:  "10.0.0.1",
		ModelName: "some-model",
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")

	// Pre-create gateway resources
	pool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	r := newTestReconciler(scheme, detector, md, pool, route)
	ctx := context.Background()

	// cleanupGatewayResources should clean up since phase != Running but gateway exists
	err := r.cleanupGatewayResources(ctx, md)
	if err != nil {
		t.Fatalf("cleanupGatewayResources failed: %v", err)
	}

	// Verify resources deleted
	var p inferencev1.InferencePool
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &p); err == nil {
		t.Error("expected InferencePool to be deleted on phase transition")
	}
	var rt gatewayv1.HTTPRoute
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &rt); err == nil {
		t.Error("expected HTTPRoute to be deleted on phase transition")
	}

	// Verify status cleared and condition set
	if md.Status.Gateway != nil {
		t.Error("expected gateway status to be nil after phase transition cleanup")
	}
	for _, c := range md.Status.Conditions {
		if c.Type == airunwayv1alpha1.ConditionTypeGatewayReady {
			if c.Status != metav1.ConditionFalse {
				t.Errorf("expected GatewayReady False after phase transition, got %s", c.Status)
			}
			return
		}
	}
	t.Error("expected GatewayReady condition to be set after phase transition")
}

func TestGateway_NotAvailableSkipsSilently(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	// Detector says CRDs not available
	detector := fakeDetector(false, "", "")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	err := r.reconcileGateway(ctx, md)
	if err != nil {
		t.Fatalf("expected no error when gateway not available, got: %v", err)
	}

	// Verify no InferencePool was created
	var pool inferencev1.InferencePool
	err = r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &pool)
	if err == nil {
		t.Error("expected InferencePool to NOT be created when gateway not available")
	}
}

func TestGateway_NilDetectorSkipsSilently(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	// No detector at all
	r := newTestReconciler(scheme, nil, md)
	ctx := context.Background()

	err := r.reconcileGateway(ctx, md)
	if err != nil {
		t.Fatalf("expected no error when detector is nil, got: %v", err)
	}
}

func TestGateway_PatchGatewayOptOut(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "model-ns")
	// Gateway in a different namespace — without patching, allowedRoutes won't be modified.
	gw := newTestGateway("my-gateway", "gateway-ns")
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	detector.PatchGateway = false // global opt-out via --patch-gateway-allowed-routes=false
	r := newTestReconciler(scheme, detector, md, gw)
	ctx := context.Background()

	err := r.reconcileGateway(ctx, md)
	if err != nil {
		t.Fatalf("reconcileGateway failed: %v", err)
	}

	// Verify Gateway listeners were NOT patched (no allowedRoutes selector added)
	var updated gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Name: "my-gateway", Namespace: "gateway-ns"}, &updated); err != nil {
		t.Fatalf("could not get gateway: %v", err)
	}
	for _, l := range updated.Spec.Listeners {
		if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil && l.AllowedRoutes.Namespaces.Selector != nil {
			t.Error("expected Gateway listeners NOT to be patched when --patch-gateway-allowed-routes=false")
		}
	}
}

func TestGateway_StatusUpdate(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, newTestGateway("my-gateway", "gateway-ns"))
	ctx := context.Background()

	err := r.reconcileGateway(ctx, md)
	if err != nil {
		t.Fatalf("reconcileGateway failed: %v", err)
	}

	// Check gateway status
	if md.Status.Gateway == nil {
		t.Fatal("expected gateway status to be set")
	}
	if md.Status.Gateway.Endpoint != "" {
		t.Errorf("expected empty endpoint when Gateway has no status address, got %q", md.Status.Gateway.Endpoint)
	}
	if md.Status.Gateway.ModelName != "meta-llama/Llama-3-8B" {
		t.Errorf("expected model name %q, got %q", "meta-llama/Llama-3-8B", md.Status.Gateway.ModelName)
	}

	// Check GatewayReady condition
	found := false
	for _, c := range md.Status.Conditions {
		if c.Type == airunwayv1alpha1.ConditionTypeGatewayReady {
			found = true
			if c.Status != metav1.ConditionTrue {
				t.Errorf("expected GatewayReady condition to be True, got %s", c.Status)
			}
		}
	}
	if !found {
		t.Error("expected GatewayReady condition to be set")
	}
}

func TestGateway_StatusEndpointFromGatewayAddress(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gateway",
			Namespace: "gateway-ns",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "istio",
		},
		Status: gatewayv1.GatewayStatus{
			Addresses: []gatewayv1.GatewayStatusAddress{
				{Value: "10.0.0.42"},
			},
		},
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, gw)
	ctx := context.Background()

	err := r.reconcileGateway(ctx, md)
	if err != nil {
		t.Fatalf("reconcileGateway failed: %v", err)
	}

	if md.Status.Gateway == nil {
		t.Fatal("expected gateway status to be set")
	}
	if md.Status.Gateway.Endpoint != "10.0.0.42" {
		t.Errorf("expected endpoint %q, got %q", "10.0.0.42", md.Status.Gateway.Endpoint)
	}
}

func TestGateway_StatusModelNameOverride(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{
		ModelName: "custom-model-name",
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, newTestGateway("my-gateway", "gateway-ns"))
	ctx := context.Background()

	err := r.reconcileGateway(ctx, md)
	if err != nil {
		t.Fatalf("reconcileGateway failed: %v", err)
	}

	if md.Status.Gateway.ModelName != "custom-model-name" {
		t.Errorf("expected model name %q, got %q", "custom-model-name", md.Status.Gateway.ModelName)
	}
}

func TestGateway_StatusServedNameFallback(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Model.ServedName = "llama-3"
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, newTestGateway("my-gateway", "gateway-ns"))
	ctx := context.Background()

	err := r.reconcileGateway(ctx, md)
	if err != nil {
		t.Fatalf("reconcileGateway failed: %v", err)
	}

	if md.Status.Gateway.ModelName != "llama-3" {
		t.Errorf("expected model name %q, got %q", "llama-3", md.Status.Gateway.ModelName)
	}
}

func TestGateway_ModelNameAutoDiscoveryFallsBackToModelID(t *testing.T) {
	// When no server is reachable, resolveModelName should fall back to spec.model.id
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{
		Service: "nonexistent-svc",
		Port:    8080,
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	name := r.resolveModelName(ctx, md)
	if name != "meta-llama/Llama-3-8B" {
		t.Errorf("expected fallback to spec.model.id %q, got %q", "meta-llama/Llama-3-8B", name)
	}
}

func TestGateway_ModelNameExplicitOverrideTakesPriority(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{
		ModelName: "my-override",
	}
	md.Spec.Model.ServedName = "should-not-use"
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{
		Service: "some-svc",
		Port:    8080,
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	name := r.resolveModelName(ctx, md)
	if name != "my-override" {
		t.Errorf("expected explicit override %q, got %q", "my-override", name)
	}
}

func TestGateway_ModelNameServedNameSkipsDiscovery(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Model.ServedName = "explicit-served"
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{
		Service: "some-svc",
		Port:    8080,
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	name := r.resolveModelName(ctx, md)
	if name != "explicit-served" {
		t.Errorf("expected served name %q, got %q", "explicit-served", name)
	}
}

func TestGateway_KaitoLlamaCppServedNameFallsBackToModelID(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "kaito"}
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeLlamaCpp
	md.Spec.Model.ServedName = "explicit-served"
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	name := r.resolveModelName(ctx, md)
	if name != "meta-llama/Llama-3-8B" {
		t.Errorf("expected fallback to spec.model.id %q, got %q", "meta-llama/Llama-3-8B", name)
	}
}

func TestGateway_ModelNameNoEndpointFallsBack(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Status.Endpoint = nil // no endpoint info
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	name := r.resolveModelName(ctx, md)
	if name != "meta-llama/Llama-3-8B" {
		t.Errorf("expected fallback to spec.model.id %q, got %q", "meta-llama/Llama-3-8B", name)
	}
}

func TestGateway_CleanupNonExistentResourcesNoError(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Status.Gateway = &airunwayv1alpha1.GatewayStatus{Endpoint: "10.0.0.1"}
	r := newTestReconciler(scheme, nil, md)
	ctx := context.Background()

	// Should not error even if resources don't exist
	err := r.cleanupGatewayResources(ctx, md)
	if err != nil {
		t.Fatalf("cleanupGatewayResources failed on non-existent resources: %v", err)
	}
	if md.Status.Gateway != nil {
		t.Error("expected gateway status to be cleared")
	}
}

// --- Provider Gateway Delegation Tests ---

// mockProviderResolver implements gateway.ProviderCapabilityResolver for testing.
type mockProviderResolver struct {
	caps map[string]*airunwayv1alpha1.GatewayCapabilities
}

func (m *mockProviderResolver) GetGatewayCapabilities(_ context.Context, providerName string) *airunwayv1alpha1.GatewayCapabilities {
	if m.caps == nil {
		return nil
	}
	return m.caps[providerName]
}

func TestResolveProviderInferencePoolName_WithPattern(t *testing.T) {
	name := resolveProviderInferencePoolName("{namespace}-{name}-pool", "llama-70b", "default")
	if name != "default-llama-70b-pool" {
		t.Errorf("expected 'default-llama-70b-pool', got %q", name)
	}
}

func TestResolveProviderInferencePoolName_EmptyPattern(t *testing.T) {
	name := resolveProviderInferencePoolName("", "llama-70b", "default")
	if name != "llama-70b" {
		t.Errorf("expected fallback to md name 'llama-70b', got %q", name)
	}
}

func TestResolveProviderInferencePoolName_NameOnlyPattern(t *testing.T) {
	name := resolveProviderInferencePoolName("{name}-pool", "llama-70b", "default")
	if name != "llama-70b-pool" {
		t.Errorf("expected 'llama-70b-pool', got %q", name)
	}
}

func TestGateway_ResolveProviderCapabilities_SpecProvider(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}

	resolver := &mockProviderResolver{
		caps: map[string]*airunwayv1alpha1.GatewayCapabilities{
			"dynamo": {InferencePoolNamespace: "dynamo-system", InferencePoolNamePattern: "{namespace}-{name}-pool"},
		},
	}

	r := newTestReconciler(scheme, nil, md)
	r.ProviderResolver = resolver

	caps, err := r.resolveProviderGatewayCapabilities(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if caps.InferencePoolNamespace != "dynamo-system" {
		t.Errorf("expected namespace 'dynamo-system', got %s", caps.InferencePoolNamespace)
	}
	if caps.InferencePoolNamePattern != "{namespace}-{name}-pool" {
		t.Errorf("expected InferencePoolNamePattern to be '{namespace}-{name}-pool', got %s", caps.InferencePoolNamePattern)
	}
}

func TestGateway_ResolveProviderCapabilities_StatusProvider(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Provider = nil
	md.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: "dynamo"}

	resolver := &mockProviderResolver{
		caps: map[string]*airunwayv1alpha1.GatewayCapabilities{
			"dynamo": {InferencePoolNamespace: "dynamo-system"},
		},
	}

	r := newTestReconciler(scheme, nil, md)
	r.ProviderResolver = resolver

	caps, err := r.resolveProviderGatewayCapabilities(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if caps.InferencePoolNamespace != "dynamo-system" {
		t.Errorf("expected namespace 'dynamo-system', got %s", caps.InferencePoolNamespace)
	}
}

func TestGateway_ResolveProviderCapabilities_NoProvider(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Provider = nil
	md.Status.Provider = nil

	r := newTestReconciler(scheme, nil, md)
	r.ProviderResolver = &mockProviderResolver{}

	_, err := r.resolveProviderGatewayCapabilities(context.Background(), md)
	if err == nil {
		t.Error("expected error when no provider is specified")
	}
}

func TestGateway_ResolveProviderCapabilities_ProviderWithNoGatewayCapabilities(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "kaito"}

	resolver := &mockProviderResolver{
		caps: map[string]*airunwayv1alpha1.GatewayCapabilities{},
	}

	r := newTestReconciler(scheme, nil, md)
	r.ProviderResolver = resolver

	_, err := r.resolveProviderGatewayCapabilities(context.Background(), md)
	if err == nil {
		t.Error("expected error when provider has no gateway capabilities")
	}
}

func TestGateway_ProviderManagedInferencePool_Found(t *testing.T) {
	scheme := newTestScheme()

	md := newModelDeployment("llama-70b", "default")

	pool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-llama-70b-pool",
			Namespace: "dynamo-system",
		},
	}

	r := newTestReconciler(scheme, nil, md, pool)

	_, err := r.reconcileProviderManagedInferencePool(context.Background(), md, "default-llama-70b-pool", "dynamo-system", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGateway_ProviderManagedInferencePool_NotFound(t *testing.T) {
	scheme := newTestScheme()

	md := newModelDeployment("llama-70b", "default")
	r := newTestReconciler(scheme, nil, md)

	_, err := r.reconcileProviderManagedInferencePool(context.Background(), md, "default-llama-70b-pool", "dynamo-system", "default")
	if err == nil {
		t.Fatal("expected error when InferencePool does not exist")
	}
}

func TestGateway_CleanupSkipsProviderManagedResources(t *testing.T) {
	scheme := newTestScheme()

	md := newModelDeployment("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
	md.Status.Gateway = &airunwayv1alpha1.GatewayStatus{
		Endpoint: "test-model.default:80",
	}

	// Create controller-managed resources that should NOT be deleted
	pool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}

	resolver := &mockProviderResolver{
		caps: map[string]*airunwayv1alpha1.GatewayCapabilities{
			"dynamo": {},
		},
	}

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, pool)
	r.ProviderResolver = resolver

	err := r.cleanupGatewayResources(context.Background(), md)
	if err != nil {
		t.Fatalf("cleanupGatewayResources failed: %v", err)
	}

	// InferencePool should still exist (provider manages it)
	var existingPool inferencev1.InferencePool
	if err := r.Get(context.Background(), types.NamespacedName{Name: "test-model", Namespace: "default"}, &existingPool); err != nil {
		t.Errorf("InferencePool should not have been deleted (provider-managed), but got error: %v", err)
	}
}

func TestGateway_CleanupDeletesControllerManagedResources(t *testing.T) {
	scheme := newTestScheme()

	md := newModelDeployment("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "kaito"}
	md.Status.Gateway = &airunwayv1alpha1.GatewayStatus{
		Endpoint: "test-model.default:80",
	}

	pool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}

	resolver := &mockProviderResolver{
		caps: map[string]*airunwayv1alpha1.GatewayCapabilities{},
	}

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, pool)
	r.ProviderResolver = resolver

	err := r.cleanupGatewayResources(context.Background(), md)
	if err != nil {
		t.Fatalf("cleanupGatewayResources failed: %v", err)
	}

	// InferencePool should be deleted (controller manages it)
	var deletedPool inferencev1.InferencePool
	if err := r.Get(context.Background(), types.NamespacedName{Name: "test-model", Namespace: "default"}, &deletedPool); err == nil {
		t.Error("InferencePool should have been deleted (controller-managed)")
	}
}

// gwWithNamespaceSelector creates a Gateway with a matchExpressions In-list for the given namespaces.
func gwWithNamespaceSelector(name, ns string, namespaces ...string) *gatewayv1.Gateway {
	fromSelector := gatewayv1.NamespacesFromSelector
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "istio",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: &fromSelector,
							Selector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "kubernetes.io/metadata.name",
										Operator: metav1.LabelSelectorOpIn,
										Values:   namespaces,
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestGateway_CleanupRevertsAllowedRoutes(t *testing.T) {
	scheme := newTestScheme()
	gw := gwWithNamespaceSelector("my-gateway", "gateway-ns", "model-ns")

	md := newModelDeployment("test-model", "model-ns")
	md.Status.Gateway = &airunwayv1alpha1.GatewayStatus{Endpoint: "10.0.0.1"}

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	detector.PatchGateway = true

	r := newTestReconciler(scheme, detector, md, gw)
	ctx := context.Background()

	err := r.cleanupGatewayResources(ctx, md)
	if err != nil {
		t.Fatalf("cleanupGatewayResources failed: %v", err)
	}

	// Verify Gateway allowedRoutes was reverted to SameNamespace
	var updatedGW gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Name: "my-gateway", Namespace: "gateway-ns"}, &updatedGW); err != nil {
		t.Fatalf("failed to get Gateway: %v", err)
	}
	for _, l := range updatedGW.Spec.Listeners {
		if l.AllowedRoutes == nil || l.AllowedRoutes.Namespaces == nil || l.AllowedRoutes.Namespaces.From == nil {
			t.Fatal("expected allowedRoutes to be set after revert")
		}
		if *l.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromSame {
			t.Errorf("expected allowedRoutes.from=Same, got %s", *l.AllowedRoutes.Namespaces.From)
		}
		if l.AllowedRoutes.Namespaces.Selector != nil {
			t.Error("expected selector to be nil after revert")
		}
	}
}

func TestGateway_CleanupKeepsAllowedRoutesWhenOtherMDExists(t *testing.T) {
	scheme := newTestScheme()
	gw := gwWithNamespaceSelector("my-gateway", "gateway-ns", "model-ns")

	md := newModelDeployment("test-model", "model-ns")
	md.Status.Gateway = &airunwayv1alpha1.GatewayStatus{Endpoint: "10.0.0.1"}

	// Another MD in the same namespace with gateway enabled (default)
	otherMD := newModelDeployment("other-model", "model-ns")

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	detector.PatchGateway = true

	r := newTestReconciler(scheme, detector, md, otherMD, gw)
	ctx := context.Background()

	err := r.cleanupGatewayResources(ctx, md)
	if err != nil {
		t.Fatalf("cleanupGatewayResources failed: %v", err)
	}

	// Verify Gateway allowedRoutes was NOT reverted (other MD still needs it)
	var updatedGW gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Name: "my-gateway", Namespace: "gateway-ns"}, &updatedGW); err != nil {
		t.Fatalf("failed to get Gateway: %v", err)
	}
	for _, l := range updatedGW.Spec.Listeners {
		if l.AllowedRoutes == nil || l.AllowedRoutes.Namespaces == nil || l.AllowedRoutes.Namespaces.From == nil {
			t.Fatal("expected allowedRoutes to still be set")
		}
		if *l.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromSelector {
			t.Errorf("expected allowedRoutes.from=Selector (kept for other MD), got %s", *l.AllowedRoutes.Namespaces.From)
		}
	}
}

func TestGateway_CleanupRemovesOneNamespaceFromMultiple(t *testing.T) {
	scheme := newTestScheme()
	// Gateway allows both dynamo-system and kaito-workspace
	gw := gwWithNamespaceSelector("my-gateway", "gateway-ns", "dynamo-system", "kaito-workspace")

	md := newModelDeployment("test-model", "dynamo-system")
	md.Status.Gateway = &airunwayv1alpha1.GatewayStatus{Endpoint: "10.0.0.1"}

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	detector.PatchGateway = true

	r := newTestReconciler(scheme, detector, md, gw)
	ctx := context.Background()

	err := r.cleanupGatewayResources(ctx, md)
	if err != nil {
		t.Fatalf("cleanupGatewayResources failed: %v", err)
	}

	// Verify only dynamo-system was removed; kaito-workspace remains
	var updatedGW gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Name: "my-gateway", Namespace: "gateway-ns"}, &updatedGW); err != nil {
		t.Fatalf("failed to get Gateway: %v", err)
	}
	for _, l := range updatedGW.Spec.Listeners {
		if l.AllowedRoutes == nil || l.AllowedRoutes.Namespaces == nil || l.AllowedRoutes.Namespaces.From == nil {
			t.Fatal("expected allowedRoutes to still be set")
		}
		if *l.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromSelector {
			t.Errorf("expected allowedRoutes.from=Selector, got %s", *l.AllowedRoutes.Namespaces.From)
		}
		sel := l.AllowedRoutes.Namespaces.Selector
		if sel == nil || len(sel.MatchExpressions) == 0 {
			t.Fatal("expected matchExpressions to be set")
		}
		values := sel.MatchExpressions[0].Values
		if len(values) != 1 || values[0] != "kaito-workspace" {
			t.Errorf("expected only [kaito-workspace] in selector values, got %v", values)
		}
	}
}

func TestGateway_EnsureAddsNamespaceToExistingSelector(t *testing.T) {
	scheme := newTestScheme()
	// Gateway already allows dynamo-system
	gw := gwWithNamespaceSelector("my-gateway", "gateway-ns", "dynamo-system")

	md := newModelDeployment("test-model", "kaito-workspace")

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	detector.PatchGateway = true

	r := newTestReconciler(scheme, detector, md, gw)
	ctx := context.Background()

	gwConfig := &gateway.GatewayConfig{GatewayName: "my-gateway", GatewayNamespace: "gateway-ns"}
	err := r.ensureGatewayAllowsNamespace(ctx, gwConfig, "kaito-workspace")
	if err != nil {
		t.Fatalf("ensureGatewayAllowsNamespace failed: %v", err)
	}

	// Verify both namespaces are now allowed
	var updatedGW gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Name: "my-gateway", Namespace: "gateway-ns"}, &updatedGW); err != nil {
		t.Fatalf("failed to get Gateway: %v", err)
	}
	for _, l := range updatedGW.Spec.Listeners {
		sel := l.AllowedRoutes.Namespaces.Selector
		if sel == nil || len(sel.MatchExpressions) == 0 {
			t.Fatal("expected matchExpressions to be set")
		}
		values := sel.MatchExpressions[0].Values
		if len(values) != 2 {
			t.Fatalf("expected 2 namespaces in selector, got %v", values)
		}
		// Values are sorted
		if values[0] != "dynamo-system" || values[1] != "kaito-workspace" {
			t.Errorf("expected [dynamo-system, kaito-workspace], got %v", values)
		}
	}
}

func TestGateway_EnsureMigratesLegacyMatchLabels(t *testing.T) {
	scheme := newTestScheme()
	// Gateway has legacy matchLabels format (single namespace)
	fromSelector := gatewayv1.NamespacesFromSelector
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "gateway-ns"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "istio",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: &fromSelector,
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"kubernetes.io/metadata.name": "dynamo-system"},
							},
						},
					},
				},
			},
		},
	}

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	detector.PatchGateway = true

	md := newModelDeployment("test-model", "kaito-workspace")
	r := newTestReconciler(scheme, detector, md, gw)
	ctx := context.Background()

	gwConfig := &gateway.GatewayConfig{GatewayName: "my-gateway", GatewayNamespace: "gateway-ns"}
	err := r.ensureGatewayAllowsNamespace(ctx, gwConfig, "kaito-workspace")
	if err != nil {
		t.Fatalf("ensureGatewayAllowsNamespace failed: %v", err)
	}

	// Verify both namespaces are now in matchExpressions
	var updatedGW gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Name: "my-gateway", Namespace: "gateway-ns"}, &updatedGW); err != nil {
		t.Fatalf("failed to get Gateway: %v", err)
	}
	for _, l := range updatedGW.Spec.Listeners {
		sel := l.AllowedRoutes.Namespaces.Selector
		if sel == nil || len(sel.MatchExpressions) == 0 {
			t.Fatal("expected matchExpressions after migration")
		}
		values := sel.MatchExpressions[0].Values
		if len(values) != 2 {
			t.Fatalf("expected 2 namespaces after migration, got %v", values)
		}
		if values[0] != "dynamo-system" || values[1] != "kaito-workspace" {
			t.Errorf("expected [dynamo-system, kaito-workspace], got %v", values)
		}
	}
}

func TestIsNoMatchError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil error", nil, false},
		{"generic error", fmt.Errorf("something failed"), false},
		{"no matches for kind", fmt.Errorf("no matches for kind \"InferencePool\" in version \"inference.networking.k8s.io/v1\""), true},
		{"server not found", fmt.Errorf("the server could not find the requested resource"), true},
		{"no kind registered", fmt.Errorf("no kind is registered for the type \"InferencePool\""), true},
		{"wrapped error", fmt.Errorf("reconciling InferencePool: %w", fmt.Errorf("no matches for kind \"InferencePool\"")), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNoMatchError(tt.err); got != tt.expected {
				t.Errorf("isNoMatchError(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}
