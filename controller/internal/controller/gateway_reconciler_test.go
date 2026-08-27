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
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/internal/gateway"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const inferencePoolNotFoundReason = "InferencePoolNotFound"

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
	c := cb.Build()
	return &ModelDeploymentReconciler{
		Client:           c,
		Scheme:           scheme,
		GatewayDetector:  detector,
		ProviderResolver: gateway.NewInferenceProviderConfigResolver(c),
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

// newBBRDeployment returns a stand-in for the shared body-based-router
// Deployment installed by the upstream GAIE helm chart. It carries an unrelated
// pod-template annotation so a wholesale rewrite is detectable, not just the
// restart annotation itself.
func newBBRDeployment(ns string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "body-based-router", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"unrelated": "keep-me"},
				},
			},
		},
	}
}

// assertBBRNotRestarted fails if anything rolled the shared BBR Deployment.
// The controller used to patch airunway.ai/restartedAt onto its pod template to
// force a restart; nothing should write to that Deployment at all now.
func assertBBRNotRestarted(t *testing.T, r *ModelDeploymentReconciler, ns string) {
	t.Helper()
	var bbr appsv1.Deployment
	key := types.NamespacedName{Name: "body-based-router", Namespace: ns}
	if err := r.Get(context.Background(), key, &bbr); err != nil {
		t.Fatalf("body-based-router Deployment not found: %v", err)
	}
	if _, found := bbr.Spec.Template.Annotations["airunway.ai/restartedAt"]; found {
		t.Errorf("BBR was restarted: pod template carries airunway.ai/restartedAt (%v)",
			bbr.Spec.Template.Annotations)
	}
	if got := bbr.Spec.Template.Annotations["unrelated"]; got != "keep-me" {
		t.Errorf("BBR pod template annotations were rewritten: %v", bbr.Spec.Template.Annotations)
	}
}

// --- Tests ---

func TestGateway_InferencePoolCreation(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	err := r.reconcileInferencePool(ctx, md, 8080)
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

// TestGateway_InferencePoolCreationDoesNotRestartBBR is a regression guard for
// #334. Creating an InferencePool used to force a rolling restart of the shared
// body-based-router Deployment, on the mistaken premise that BBR builds a model
// registry at startup. It does not — its body-field-to-header plugin sets the
// model header per request, and its ServiceAccount is only granted read access
// to ConfigMaps — so the restart bought nothing while opening a window in which
// an already-serving model could mis-route. Nothing should touch BBR now.
func TestGateway_InferencePoolCreationDoesNotRestartBBR(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	// The removed code resolved BBR's namespace from the Gateway, so seed the
	// Deployment where a restart would have gone looking for it.
	bbr := newBBRDeployment("gateway-ns")
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, bbr)
	ctx := context.Background()

	if err := r.reconcileInferencePool(ctx, md, 8080); err != nil {
		t.Fatalf("reconcileInferencePool failed: %v", err)
	}

	// Confirm the pool was actually created, otherwise the restart path never
	// ran and this test would pass vacuously.
	var pool inferencev1.InferencePool
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &pool); err != nil {
		t.Fatalf("InferencePool not created, so this test proves nothing: %v", err)
	}

	assertBBRNotRestarted(t, r, "gateway-ns")
}

// TestGateway_ProviderManagedPoolDoesNotRestartBBR covers the second removed
// call site (#334). Provider-managed pools are created by the provider's own
// operator, so there is no "created" signal; the restart was gated on a
// one-shot airunway.ai/bbr-restarted annotation written back to the
// ModelDeployment instead. Neither the restart nor that annotation should
// happen now.
func TestGateway_ProviderManagedPoolDoesNotRestartBBR(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	bbr := newBBRDeployment("gateway-ns")
	pool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-pool", Namespace: "default"},
		Spec: inferencev1.InferencePoolSpec{
			EndpointPickerRef: inferencev1.EndpointPickerRef{
				Name: inferencev1.ObjectName("provider-pool-epp"),
			},
		},
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, bbr, pool)
	ctx := context.Background()

	eppName, err := r.reconcileProviderManagedInferencePool(ctx, md, "provider-pool", "default")
	if err != nil {
		t.Fatalf("reconcileProviderManagedInferencePool failed: %v", err)
	}
	if eppName != "provider-pool-epp" {
		t.Errorf("expected EPP name %q, got %q", "provider-pool-epp", eppName)
	}

	assertBBRNotRestarted(t, r, "gateway-ns")

	var got airunwayv1alpha1.ModelDeployment
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &got); err != nil {
		t.Fatalf("ModelDeployment not found: %v", err)
	}
	if _, found := got.Annotations["airunway.ai/bbr-restarted"]; found {
		t.Error("ModelDeployment carries the removed airunway.ai/bbr-restarted annotation")
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
	err := r.reconcileInferencePool(ctx, md, 8000)
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

func TestGateway_UserProvidedPoolRefIsUsedWithoutMutation(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{
		PoolRef:   "shared-pool",
		ModelName: "shared-model",
	}

	pool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-pool", Namespace: "default"},
		Spec: inferencev1.InferencePoolSpec{
			Selector: inferencev1.LabelSelector{
				MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{"routing.example.com/tier": "gold"},
			},
			TargetPorts: []inferencev1.Port{{Number: 9090}},
			EndpointPickerRef: inferencev1.EndpointPickerRef{
				Name: inferencev1.ObjectName("shared-epp"),
			},
		},
	}
	replicas := int32(3)
	epp := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-epp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "epp", Image: "example.com/custom-epp:v1"}}},
			},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model-svc", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "test-model"}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model-pod", Namespace: "default", Labels: map[string]string{"app": "test-model"}},
	}

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, pool, epp, service, pod)
	r.ProviderResolver = &mockProviderResolver{
		caps: map[string]*airunwayv1alpha1.GatewayCapabilities{
			"dynamo": {
				ManagesInferencePool:     true,
				InferencePoolNamePattern: "{name}-provider-pool",
				InferencePoolNamespace:   "{namespace}",
			},
		},
	}
	ctx := context.Background()

	if err := r.reconcileGateway(ctx, md); err != nil {
		t.Fatalf("reconcileGateway failed: %v", err)
	}

	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &route); err != nil {
		t.Fatalf("HTTPRoute not found: %v", err)
	}
	backend := route.Spec.Rules[0].BackendRefs[0].BackendObjectReference
	if backend.Name != "shared-pool" {
		t.Errorf("expected backend ref name %q, got %q", "shared-pool", backend.Name)
	}
	if backend.Namespace == nil || *backend.Namespace != "default" {
		t.Errorf("expected backend namespace %q, got %v", "default", backend.Namespace)
	}

	var gotPool inferencev1.InferencePool
	if err := r.Get(ctx, types.NamespacedName{Name: "shared-pool", Namespace: "default"}, &gotPool); err != nil {
		t.Fatalf("user-provided InferencePool not found after reconcile: %v", err)
	}
	if gotPool.Spec.TargetPorts[0].Number != 9090 || gotPool.Spec.EndpointPickerRef.Name != "shared-epp" {
		t.Errorf("user-provided InferencePool was mutated: %+v", gotPool.Spec)
	}
	if len(gotPool.OwnerReferences) != 0 {
		t.Errorf("user-provided InferencePool gained owner references: %v", gotPool.OwnerReferences)
	}

	var gotEPP appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: "shared-epp", Namespace: "default"}, &gotEPP); err != nil {
		t.Fatalf("user-provided EPP not found after reconcile: %v", err)
	}
	if gotEPP.Spec.Replicas == nil || *gotEPP.Spec.Replicas != 3 || gotEPP.Spec.Template.Spec.Containers[0].Image != "example.com/custom-epp:v1" {
		t.Errorf("user-provided EPP was mutated: %+v", gotEPP.Spec)
	}

	var gotPod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &gotPod); err != nil {
		t.Fatalf("model pod not found after reconcile: %v", err)
	}
	if _, found := gotPod.Labels[airunwayv1alpha1.LabelModelDeployment]; found {
		t.Errorf("model pod was labeled for a user-provided InferencePool: %v", gotPod.Labels)
	}

	var controllerPool inferencev1.InferencePool
	if err := r.Get(ctx, types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &controllerPool); !apierrors.IsNotFound(err) {
		t.Errorf("expected no controller-managed InferencePool, got %v", err)
	}
	var controllerEPP appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: md.Name + "-epp", Namespace: md.Namespace}, &controllerEPP); !apierrors.IsNotFound(err) {
		t.Errorf("expected no controller-managed EPP, got %v", err)
	}
}

func TestGateway_UserProvidedPoolPreservesUnownedGeneratedNameResources(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{PoolRef: "shared-pool"}

	referencedPool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-pool", Namespace: md.Namespace},
		Spec: inferencev1.InferencePoolSpec{
			TargetPorts: []inferencev1.Port{{Number: 9090}},
			EndpointPickerRef: inferencev1.EndpointPickerRef{
				Name: inferencev1.ObjectName(md.Name + "-epp"),
			},
		},
	}
	unownedGeneratedPool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: md.Name, Namespace: md.Namespace},
		Spec: inferencev1.InferencePoolSpec{
			TargetPorts: []inferencev1.Port{{Number: 7777}},
		},
	}
	replicas := int32(4)
	referencedEPP := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: md.Name + "-epp", Namespace: md.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "epp", Image: "example.com/user-epp:v1"}}},
			},
		},
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, referencedPool, unownedGeneratedPool, referencedEPP)
	ctx := context.Background()

	if err := r.reconcileGateway(ctx, md); err != nil {
		t.Fatalf("reconcileGateway failed: %v", err)
	}

	var gotGeneratedPool inferencev1.InferencePool
	if err := r.Get(ctx, client.ObjectKeyFromObject(unownedGeneratedPool), &gotGeneratedPool); err != nil {
		t.Fatalf("unowned generated-name InferencePool was deleted: %v", err)
	}
	if gotGeneratedPool.Spec.TargetPorts[0].Number != 7777 {
		t.Errorf("unowned generated-name InferencePool was mutated: %+v", gotGeneratedPool.Spec)
	}
	var gotEPP appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(referencedEPP), &gotEPP); err != nil {
		t.Fatalf("referenced generated-name EPP was deleted: %v", err)
	}
	if gotEPP.Spec.Replicas == nil || *gotEPP.Spec.Replicas != 4 || gotEPP.Spec.Template.Spec.Containers[0].Image != "example.com/user-epp:v1" {
		t.Errorf("referenced generated-name EPP was mutated: %+v", gotEPP.Spec)
	}
}

func TestGateway_SwitchToUserProvidedPoolCleansControllerOwnedResources(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	referencedPool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-pool", Namespace: "default"},
		Spec: inferencev1.InferencePoolSpec{
			Selector: inferencev1.LabelSelector{
				MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{"routing.example.com/tier": "gold"},
			},
			TargetPorts: []inferencev1.Port{{Number: 9090}},
			EndpointPickerRef: inferencev1.EndpointPickerRef{
				Name: inferencev1.ObjectName("shared-epp"),
			},
		},
	}
	replicas := int32(3)
	referencedEPP := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-epp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "epp", Image: "example.com/custom-epp:v1"}}},
			},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model-svc", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "test-model"}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model-pod", Namespace: "default", Labels: map[string]string{"app": "test-model"}},
	}

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, referencedPool, referencedEPP, service, pod)
	ctx := context.Background()

	// Start in controller-managed mode so every stale resource exists before
	// poolRef takes ownership of the gateway backend.
	if err := r.reconcileGateway(ctx, md); err != nil {
		t.Fatalf("initial managed reconcile failed: %v", err)
	}
	preservedLabelPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-model-pod",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelModelDeployment: "other-model",
			},
		},
	}
	if err := r.Create(ctx, preservedLabelPod); err != nil {
		t.Fatalf("create pod with unrelated model label: %v", err)
	}

	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{PoolRef: referencedPool.Name}
	if err := r.reconcileGateway(ctx, md); err != nil {
		t.Fatalf("user-provided pool reconcile failed: %v", err)
	}

	controllerResources := []client.Object{
		&inferencev1.InferencePool{ObjectMeta: metav1.ObjectMeta{Name: md.Name, Namespace: md.Namespace}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: md.Name + "-epp", Namespace: md.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: md.Name + "-epp", Namespace: md.Namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: md.Name + "-epp", Namespace: md.Namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: md.Name + "-epp", Namespace: md.Namespace}},
	}
	for _, resource := range controllerResources {
		if err := r.Get(ctx, client.ObjectKeyFromObject(resource), resource); !apierrors.IsNotFound(err) {
			t.Errorf("expected controller-owned %T to be deleted, got %v", resource, err)
		}
	}

	var gotPool inferencev1.InferencePool
	if err := r.Get(ctx, client.ObjectKeyFromObject(referencedPool), &gotPool); err != nil {
		t.Fatalf("referenced InferencePool was deleted: %v", err)
	}
	if gotPool.Spec.TargetPorts[0].Number != 9090 || gotPool.Spec.EndpointPickerRef.Name != "shared-epp" {
		t.Errorf("referenced InferencePool was mutated: %+v", gotPool.Spec)
	}
	var gotEPP appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(referencedEPP), &gotEPP); err != nil {
		t.Fatalf("referenced EPP was deleted: %v", err)
	}
	if gotEPP.Spec.Replicas == nil || *gotEPP.Spec.Replicas != 3 || gotEPP.Spec.Template.Spec.Containers[0].Image != "example.com/custom-epp:v1" {
		t.Errorf("referenced EPP was mutated: %+v", gotEPP.Spec)
	}

	var gotPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKeyFromObject(pod), &gotPod); err != nil {
		t.Fatalf("model pod not found: %v", err)
	}
	if _, found := gotPod.Labels[airunwayv1alpha1.LabelModelDeployment]; found {
		t.Errorf("controller model label was not removed: %v", gotPod.Labels)
	}
	var gotPreservedLabelPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKeyFromObject(preservedLabelPod), &gotPreservedLabelPod); err != nil {
		t.Fatalf("pod with unrelated model label not found: %v", err)
	}
	if got := gotPreservedLabelPod.Labels[airunwayv1alpha1.LabelModelDeployment]; got != "other-model" {
		t.Errorf("unrelated model label was changed: %q", got)
	}

	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &route); err != nil {
		t.Fatalf("controller-managed HTTPRoute should be updated, not deleted: %v", err)
	}
	backend := route.Spec.Rules[0].BackendRefs[0].BackendObjectReference
	if backend.Name != gatewayv1.ObjectName(referencedPool.Name) {
		t.Errorf("expected route backend %q, got %q", referencedPool.Name, backend.Name)
	}
}

func TestGateway_MissingUserProvidedPoolRetiresManagedRouteUntilRecovery(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model-svc", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "test-model"}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model-pod", Namespace: "default", Labels: map[string]string{"app": "test-model"}},
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, service, pod)
	ctx := context.Background()

	if err := r.reconcileGateway(ctx, md); err != nil {
		t.Fatalf("initial managed reconcile failed: %v", err)
	}
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{PoolRef: "missing-pool"}
	if err := r.Update(ctx, md); err != nil {
		t.Fatalf("persist poolRef transition: %v", err)
	}
	if err := r.reconcileGateway(ctx, md); err != nil {
		t.Fatalf("missing pool reconcile failed: %v", err)
	}

	var route gatewayv1.HTTPRoute
	routeKey := types.NamespacedName{Name: md.Name, Namespace: md.Namespace}
	if err := r.Get(ctx, routeKey, &route); !apierrors.IsNotFound(err) {
		t.Fatalf("expected stale controller-managed HTTPRoute to be deleted, got %v", err)
	}
	if md.Annotations[airunwayv1alpha1.HTTPRouteCreated] != "" {
		t.Errorf("expected HTTPRoute creation annotation to be cleared, got %v", md.Annotations)
	}
	var storedMD airunwayv1alpha1.ModelDeployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(md), &storedMD); err != nil {
		t.Fatalf("get ModelDeployment after route retirement: %v", err)
	}
	if storedMD.Annotations[airunwayv1alpha1.HTTPRouteCreated] != "" {
		t.Errorf("expected persisted HTTPRoute creation annotation to be cleared, got %v", storedMD.Annotations)
	}

	recoveredPool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-pool", Namespace: md.Namespace},
	}
	if err := r.Create(ctx, recoveredPool); err != nil {
		t.Fatalf("create recovered InferencePool: %v", err)
	}
	if err := r.reconcileGateway(ctx, md); err != nil {
		t.Fatalf("reconcile after pool recovery failed: %v", err)
	}
	route = gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, routeKey, &route); err != nil {
		t.Fatalf("expected HTTPRoute to be recreated after pool recovery: %v", err)
	}
	backend := route.Spec.Rules[0].BackendRefs[0].BackendObjectReference
	if backend.Name != "missing-pool" {
		t.Errorf("expected recovered route backend %q, got %q", "missing-pool", backend.Name)
	}
}

func TestGateway_MissingUserProvidedPoolPreservesReferencedHTTPRoute(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Annotations = map[string]string{airunwayv1alpha1.HTTPRouteCreated: "true"}
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{
		PoolRef:      "missing-pool",
		HTTPRouteRef: "custom-route",
	}
	managedRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: md.Name, Namespace: md.Namespace},
	}
	if err := ctrl.SetControllerReference(md, managedRoute, scheme); err != nil {
		t.Fatalf("set managed route controller reference: %v", err)
	}
	referencedRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: md.Spec.Gateway.HTTPRouteRef, Namespace: md.Namespace},
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, managedRoute, referencedRoute)
	ctx := context.Background()

	if err := r.reconcileGateway(ctx, md); err != nil {
		t.Fatalf("missing pool reconcile failed: %v", err)
	}
	var gotManagedRoute gatewayv1.HTTPRoute
	if err := r.Get(ctx, client.ObjectKeyFromObject(managedRoute), &gotManagedRoute); !apierrors.IsNotFound(err) {
		t.Errorf("expected stale managed route to be deleted, got %v", err)
	}
	var gotReferencedRoute gatewayv1.HTTPRoute
	if err := r.Get(ctx, client.ObjectKeyFromObject(referencedRoute), &gotReferencedRoute); err != nil {
		t.Errorf("referenced HTTPRoute was deleted: %v", err)
	}
}

func TestGateway_MissingUserProvidedPoolKeepsManagedRouteWhenMarkerClearFails(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Annotations = map[string]string{airunwayv1alpha1.HTTPRouteCreated: "true"}
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{PoolRef: "missing-pool"}
	managedRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: md.Name, Namespace: md.Namespace},
	}
	if err := ctrl.SetControllerReference(md, managedRoute, scheme); err != nil {
		t.Fatalf("set managed route controller reference: %v", err)
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&airunwayv1alpha1.ModelDeployment{}).
		WithObjects(md, managedRoute).
		Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*airunwayv1alpha1.ModelDeployment); ok {
				return errors.New("marker patch failed")
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := &ModelDeploymentReconciler{
		Client:           c,
		Scheme:           scheme,
		GatewayDetector:  detector,
		ProviderResolver: gateway.NewInferenceProviderConfigResolver(c),
	}

	err := r.reconcileGateway(context.Background(), md)
	if err == nil || !strings.Contains(err.Error(), "marker patch failed") {
		t.Fatalf("expected marker patch failure, got %v", err)
	}
	var gotRoute gatewayv1.HTTPRoute
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(managedRoute), &gotRoute); err != nil {
		t.Errorf("managed route was deleted despite marker patch failure: %v", err)
	}
}

func TestGateway_UserProvidedPoolRefNotFound(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{PoolRef: "missing-pool"}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)

	err := r.reconcileGateway(context.Background(), md)
	if err != nil {
		t.Fatalf("expected missing pool to be surfaced as a condition, got %v", err)
	}

	condition := meta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeGatewayReady)
	if condition == nil {
		t.Fatal("expected GatewayReady condition")
	}
	if condition.Status != metav1.ConditionFalse || condition.Reason != inferencePoolNotFoundReason {
		t.Errorf("expected GatewayReady=False/InferencePoolNotFound, got %s/%s", condition.Status, condition.Reason)
	}
	if !strings.Contains(condition.Message, "missing-pool") || !strings.Contains(condition.Message, "default") {
		t.Errorf("expected actionable missing pool message, got %q", condition.Message)
	}

	var route gatewayv1.HTTPRoute
	if err := r.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &route); !apierrors.IsNotFound(err) {
		t.Errorf("expected no HTTPRoute for a missing pool, got %v", err)
	}
}

func TestGateway_UserProvidedPoolRefNotFoundConditionPersistsThroughReconcile(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "vllm"}
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{PoolRef: "missing-pool"}
	md.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: "vllm"}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: md.Name, Namespace: md.Namespace}})
	if err != nil {
		t.Fatalf("expected missing pool to be surfaced without a reconcile error, got %v", err)
	}

	var persisted airunwayv1alpha1.ModelDeployment
	if err := r.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &persisted); err != nil {
		t.Fatalf("get reconciled ModelDeployment: %v", err)
	}
	condition := meta.FindStatusCondition(persisted.Status.Conditions, airunwayv1alpha1.ConditionTypeGatewayReady)
	if condition == nil {
		t.Fatal("expected persisted GatewayReady condition")
	}
	if condition.Status != metav1.ConditionFalse || condition.Reason != inferencePoolNotFoundReason {
		t.Errorf("expected persisted GatewayReady=False/InferencePoolNotFound, got %s/%s", condition.Status, condition.Reason)
	}
}

func TestGateway_UserProvidedPoolRefSurfacesRejectedStatus(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{PoolRef: "shared-pool"}
	pool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-pool", Namespace: "default", Generation: 2},
		Status: inferencev1.InferencePoolStatus{Parents: []inferencev1.ParentStatus{{
			ParentRef: inferencev1.ParentReference{
				Name:      inferencev1.ObjectName("my-gateway"),
				Namespace: inferencev1.Namespace("gateway-ns"),
			},
			Conditions: []metav1.Condition{{
				Type:               "ResolvedRefs",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: 2,
				Reason:             "BackendNotFound",
				Message:            "endpoint picker Service does not exist",
			}},
		}}},
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, pool)

	err := r.reconcileGateway(context.Background(), md)
	if err != nil {
		t.Fatalf("expected rejected pool status to be surfaced without blocking route reconciliation, got %v", err)
	}

	condition := meta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeGatewayReady)
	if condition == nil {
		t.Fatal("expected GatewayReady condition")
	}
	if condition.Status != metav1.ConditionFalse || condition.Reason != "InferencePoolNotReady" {
		t.Errorf("expected GatewayReady=False/InferencePoolNotReady, got %s/%s", condition.Status, condition.Reason)
	}
	if !strings.Contains(condition.Message, "ResolvedRefs=False") || !strings.Contains(condition.Message, "endpoint picker Service does not exist") {
		t.Errorf("expected rejected condition details, got %q", condition.Message)
	}

	var route gatewayv1.HTTPRoute
	if err := r.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &route); err != nil {
		t.Fatalf("expected HTTPRoute to converge despite rejected pool status: %v", err)
	}
	backend := route.Spec.Rules[0].BackendRefs[0].BackendObjectReference
	if backend.Name != "shared-pool" {
		t.Errorf("expected backend ref name %q, got %q", "shared-pool", backend.Name)
	}
}

func TestGateway_DynamoMockerSkipsCreation(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	// Gateway is left at its default (enabled) and the GAIE CRDs are available,
	// so only the mocker annotation should keep the controller off the gateway
	// path. The dynamo standalone-Frontend mocker DGD never creates a
	// provider-managed InferencePool, so engaging gateway would loop on NotFound.
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
	md.Annotations = map[string]string{"airunway.ai/dynamo-test-backend": "mocker"}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	if err := r.reconcileGateway(ctx, md); err != nil {
		t.Fatalf("reconcileGateway failed: %v", err)
	}

	// No InferencePool should be created.
	var pool inferencev1.InferencePool
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &pool); err == nil {
		t.Error("expected InferencePool to NOT be created in dynamo mocker mode")
	}

	// No HTTPRoute should be created.
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model", Namespace: "default"}, &route); err == nil {
		t.Error("expected HTTPRoute to NOT be created in dynamo mocker mode")
	}

	// And no GatewayReady condition should have been set (neither true nor false).
	if c := meta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeGatewayReady); c != nil {
		t.Errorf("expected no GatewayReady condition in mocker mode, got %q/%q", c.Status, c.Reason)
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

func TestGateway_CleanupPreservesUserProvidedPoolAndEPP(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{PoolRef: "test-model"}
	md.Status.Gateway = &airunwayv1alpha1.GatewayStatus{Endpoint: "gateway.example.com"}

	pool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: md.Spec.Gateway.PoolRef, Namespace: md.Namespace},
		Spec: inferencev1.InferencePoolSpec{
			EndpointPickerRef: inferencev1.EndpointPickerRef{Name: inferencev1.ObjectName(md.Name + "-epp")},
		},
	}
	eppDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: md.Name + "-epp", Namespace: md.Namespace}}
	eppService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: md.Name + "-epp", Namespace: md.Namespace}}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: md.Name, Namespace: md.Namespace}}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, pool, eppDeployment, eppService, route)
	ctx := context.Background()

	if err := r.cleanupGatewayResources(ctx, md); err != nil {
		t.Fatalf("cleanupGatewayResources failed: %v", err)
	}

	for _, obj := range []client.Object{
		&inferencev1.InferencePool{},
		&appsv1.Deployment{},
		&corev1.Service{},
	} {
		name := md.Spec.Gateway.PoolRef
		if _, ok := obj.(*inferencev1.InferencePool); !ok {
			name = md.Name + "-epp"
		}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: md.Namespace}, obj); err != nil {
			t.Errorf("user-owned %T was deleted during cleanup: %v", obj, err)
		}
	}

	var deletedRoute gatewayv1.HTTPRoute
	if err := r.Get(ctx, types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &deletedRoute); !apierrors.IsNotFound(err) {
		t.Errorf("expected controller-managed HTTPRoute to be deleted, got %v", err)
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
	// Provider declares ignoresServedName=true for its llamacpp engine, so
	// gateway routing should fall back to spec.model.id.
	ipc := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "kaito"},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			Capabilities: &airunwayv1alpha1.ProviderCapabilities{
				Engines: []airunwayv1alpha1.EngineCapability{
					{
						Name: airunwayv1alpha1.EngineTypeLlamaCpp,
						Gateway: &airunwayv1alpha1.GatewayCapabilities{
							IgnoresServedName: true,
						},
					},
				},
			},
		},
	}
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, ipc)
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

func (m *mockProviderResolver) GetGatewayCapabilities(_ context.Context, providerName string, _ airunwayv1alpha1.EngineType) *airunwayv1alpha1.GatewayCapabilities {
	if m.caps == nil {
		return nil
	}
	return m.caps[providerName]
}

func TestResolveProviderPoolField_WithPattern(t *testing.T) {
	name := resolveProviderPoolField("{namespace}-{name}-pool", "llama-70b", "default", "llama-70b")
	if name != "default-llama-70b-pool" {
		t.Errorf("expected 'default-llama-70b-pool', got %q", name)
	}
}

func TestResolveProviderPoolField_EmptyPattern_UsesFallback(t *testing.T) {
	// Name caller: fallback is md.Name.
	name := resolveProviderPoolField("", "llama-70b", "default", "llama-70b")
	if name != "llama-70b" {
		t.Errorf("expected fallback to md name 'llama-70b', got %q", name)
	}
	// Namespace caller: fallback is md.Namespace. This is the regression case —
	// previously the helper returned md.Name for both, producing a bogus
	// {Namespace: <md.Name>} lookup.
	ns := resolveProviderPoolField("", "llama-70b", "default", "default")
	if ns != "default" {
		t.Errorf("expected fallback to md namespace 'default', got %q", ns)
	}
}

func TestResolveProviderPoolField_NameOnlyPattern(t *testing.T) {
	name := resolveProviderPoolField("{name}-pool", "llama-70b", "default", "llama-70b")
	if name != "llama-70b-pool" {
		t.Errorf("expected 'llama-70b-pool', got %q", name)
	}
}

func TestGateway_CapabilitiesWithoutManagesInferencePoolStillCreatesPool(t *testing.T) {
	// Regression: a provider may declare GatewayCapabilities purely for
	// signals like IgnoresServedName (KAITO + llamacpp) without actually
	// owning the InferencePool. The controller must still create the
	// InferencePool itself in that case — otherwise it waits forever for a
	// provider-managed pool that never appears, exactly the e2e-gateway
	// failure mode prior to this fix.
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "kaito"}
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeLlamaCpp

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md, newTestGateway("my-gateway", "gateway-ns"))
	r.ProviderResolver = &mockProviderResolver{
		caps: map[string]*airunwayv1alpha1.GatewayCapabilities{
			// KAITO shape: gateway caps present, but ManagesInferencePool false.
			"kaito": {IgnoresServedName: true},
		},
	}

	if err := r.reconcileGateway(context.Background(), md); err != nil {
		t.Fatalf("reconcileGateway failed: %v", err)
	}

	var pool inferencev1.InferencePool
	if err := r.Get(context.Background(), types.NamespacedName{Name: "test-model", Namespace: "default"}, &pool); err != nil {
		t.Fatalf("expected controller-managed InferencePool 'default/test-model', got error: %v", err)
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

	// A provider that declares no gateway capabilities is a legitimate
	// no-op state, not an error: callers proceed with the default
	// InferencePool/EPP path. The resolver should return (nil, nil).
	caps, err := r.resolveProviderGatewayCapabilities(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error for provider without gateway capabilities: %v", err)
	}
	if caps != nil {
		t.Errorf("expected nil capabilities for provider with no gateway capabilities, got %+v", caps)
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

	_, err := r.reconcileProviderManagedInferencePool(context.Background(), md, "default-llama-70b-pool", "dynamo-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGateway_ProviderManagedInferencePool_NotFound(t *testing.T) {
	scheme := newTestScheme()

	md := newModelDeployment("llama-70b", "default")
	r := newTestReconciler(scheme, nil, md)

	_, err := r.reconcileProviderManagedInferencePool(context.Background(), md, "default-llama-70b-pool", "dynamo-system")
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
			"dynamo": {ManagesInferencePool: true, InferencePoolNamePattern: "{name}-pool", InferencePoolNamespace: "{namespace}"},
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

// gwWithSameNamespace creates a Gateway whose single listener is at the default
// `from: Same` (no Selector) — the pristine state before any cross-namespace MD.
func gwWithSameNamespace(name, ns string) *gatewayv1.Gateway {
	fromSame := gatewayv1.NamespacesFromSame
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
							From: &fromSame,
						},
					},
				},
			},
		},
	}
}

// TestGateway_EnsurePreservesGatewayNamespaceOnSameToSelector is the direct
// regression for issue #333: converting a listener from `from: Same` to
// `from: Selector` must keep the Gateway's own namespace in the In-list, or every
// HTTPRoute living alongside the Gateway is evicted. Table-driven across the ways
// a Gateway can implicitly be at "Same": an explicit `from: Same` listener, a
// listener with no AllowedRoutes block at all, and a multi-listener Gateway (to
// prove every listener is patched, not just the first).
func TestGateway_EnsurePreservesGatewayNamespaceOnSameToSelector(t *testing.T) {
	newSameListener := func() gatewayv1.Listener {
		fromSame := gatewayv1.NamespacesFromSame
		return gatewayv1.Listener{
			Name:          "http",
			Port:          80,
			Protocol:      gatewayv1.HTTPProtocolType,
			AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &fromSame}},
		}
	}

	tests := []struct {
		name string
		gw   *gatewayv1.Gateway
	}{
		{
			name: "explicit from: Same",
			gw:   gwWithSameNamespace("my-gateway", "gateway-ns"),
		},
		{
			name: "listener with nil AllowedRoutes",
			gw: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "gateway-ns"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "istio",
					Listeners: []gatewayv1.Listener{
						{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
					},
				},
			},
		},
		{
			name: "two listeners both at Same",
			gw: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "gateway-ns"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "istio",
					Listeners: []gatewayv1.Listener{
						func() gatewayv1.Listener { l := newSameListener(); l.Name = "http"; l.Port = 80; return l }(),
						func() gatewayv1.Listener {
							l := newSameListener()
							l.Name = "https"
							l.Port = 443
							l.Protocol = gatewayv1.HTTPSProtocolType
							return l
						}(),
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newTestScheme()
			md := newModelDeployment("test-model", "team-b")
			detector := fakeDetector(true, "my-gateway", "gateway-ns")
			detector.PatchGateway = true

			r := newTestReconciler(scheme, detector, md, tc.gw)
			ctx := context.Background()

			gwConfig := &gateway.GatewayConfig{GatewayName: "my-gateway", GatewayNamespace: "gateway-ns"}
			if err := r.ensureGatewayAllowsNamespace(ctx, gwConfig, "team-b"); err != nil {
				t.Fatalf("ensureGatewayAllowsNamespace failed: %v", err)
			}

			var updatedGW gatewayv1.Gateway
			if err := r.Get(ctx, types.NamespacedName{Name: "my-gateway", Namespace: "gateway-ns"}, &updatedGW); err != nil {
				t.Fatalf("failed to get Gateway: %v", err)
			}
			if len(updatedGW.Spec.Listeners) == 0 {
				t.Fatal("expected at least one listener")
			}
			// Every listener must be converted to Selector with BOTH the Gateway's
			// own namespace (retained) and the new cross-namespace one.
			for i, l := range updatedGW.Spec.Listeners {
				if l.AllowedRoutes == nil || l.AllowedRoutes.Namespaces == nil || l.AllowedRoutes.Namespaces.From == nil {
					t.Fatalf("listener[%d]: expected allowedRoutes to be set", i)
				}
				if *l.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromSelector {
					t.Errorf("listener[%d]: expected from=Selector, got %s", i, *l.AllowedRoutes.Namespaces.From)
				}
				sel := l.AllowedRoutes.Namespaces.Selector
				if sel == nil || len(sel.MatchExpressions) == 0 {
					t.Fatalf("listener[%d]: expected matchExpressions to be set", i)
				}
				values := sel.MatchExpressions[0].Values
				if len(values) != 2 || values[0] != "gateway-ns" || values[1] != "team-b" {
					t.Errorf("listener[%d]: expected [gateway-ns, team-b], got %v", i, values)
				}
			}
		})
	}
}

// TestGateway_EnsureRetriesOnConflictAndPreservesUnion proves the read-modify-write
// is race-safe: when a concurrent writer adds a namespace and bumps the
// resourceVersion between our Get and Patch, the optimistic-lock Patch conflicts,
// the RetryOnConflict loop re-reads fresh state, and our add is UNIONED with the
// concurrent writer's namespace rather than clobbering it.
func TestGateway_EnsureRetriesOnConflictAndPreservesUnion(t *testing.T) {
	scheme := newTestScheme()
	// Gateway starts allowing gateway-ns and team-a (a prior cross-ns writer).
	gw := gwWithNamespaceSelector("my-gateway", "gateway-ns", "gateway-ns", "team-a")
	md := newModelDeployment("test-model", "team-b")

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	detector.PatchGateway = true

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&airunwayv1alpha1.ModelDeployment{}).
		WithObjects(md, gw).
		Build()

	var patchCalls, getCalls int
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*gatewayv1.Gateway); ok {
				getCalls++
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			g, ok := obj.(*gatewayv1.Gateway)
			if !ok {
				return cl.Patch(ctx, obj, patch, opts...)
			}
			patchCalls++
			if patchCalls == 1 {
				// Simulate a concurrent writer landing first: add team-c directly
				// to the stored Gateway (bumping its resourceVersion), then reject
				// our stale-resourceVersion patch with a 409.
				var stored gatewayv1.Gateway
				if err := base.Get(ctx, client.ObjectKeyFromObject(g), &stored); err != nil {
					return err
				}
				fromSelector := gatewayv1.NamespacesFromSelector
				storedBase := stored.DeepCopy()
				for i := range stored.Spec.Listeners {
					stored.Spec.Listeners[i].AllowedRoutes = &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: &fromSelector,
							Selector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      "kubernetes.io/metadata.name",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"gateway-ns", "team-a", "team-c"},
								}},
							},
						},
					}
				}
				if err := base.Patch(ctx, &stored, client.MergeFrom(storedBase)); err != nil {
					return err
				}
				return apierrors.NewConflict(
					schema.GroupResource{Group: "gateway.networking.k8s.io", Resource: "gateways"},
					g.Name, errors.New("resourceVersion mismatch"))
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})

	r := &ModelDeploymentReconciler{
		Client:           c,
		Scheme:           scheme,
		GatewayDetector:  detector,
		ProviderResolver: gateway.NewInferenceProviderConfigResolver(c),
	}
	ctx := context.Background()

	gwConfig := &gateway.GatewayConfig{GatewayName: "my-gateway", GatewayNamespace: "gateway-ns"}
	if err := r.ensureGatewayAllowsNamespace(ctx, gwConfig, "team-b"); err != nil {
		t.Fatalf("ensureGatewayAllowsNamespace failed: %v", err)
	}

	if patchCalls < 2 {
		t.Errorf("expected at least 2 patch attempts (1 conflict + 1 success), got %d", patchCalls)
	}
	if getCalls < 2 {
		t.Errorf("expected at least 2 gets (initial + post-conflict re-read), got %d", getCalls)
	}

	// The concurrent writer's team-c AND our team-b must both survive, alongside
	// the retained gateway-ns and the pre-existing team-a.
	var finalGW gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Name: "my-gateway", Namespace: "gateway-ns"}, &finalGW); err != nil {
		t.Fatalf("failed to get Gateway: %v", err)
	}
	for _, l := range finalGW.Spec.Listeners {
		sel := l.AllowedRoutes.Namespaces.Selector
		if sel == nil || len(sel.MatchExpressions) == 0 {
			t.Fatal("expected matchExpressions to be set")
		}
		values := sel.MatchExpressions[0].Values
		want := []string{"gateway-ns", "team-a", "team-b", "team-c"}
		if len(values) != len(want) {
			t.Fatalf("expected %v, got %v", want, values)
		}
		for i := range want {
			if values[i] != want[i] {
				t.Errorf("expected %v, got %v", want, values)
				break
			}
		}
	}
}

// TestGateway_EnsureRetriesOnRealOptimisticLockConflict is the companion to
// TestGateway_EnsureRetriesOnConflictAndPreservesUnion. That test injects the 409
// synthetically, so it would still pass if MergeFromWithOptimisticLock were
// downgraded to a plain MergeFrom. This test instead lets the fake client's OWN
// optimistic-lock machinery raise the conflict: a concurrent writer bumps the
// stored resourceVersion after our helper's first Get, so the subsequent
// optimistic-lock Patch carries a stale resourceVersion and the client rejects it
// with a real 409 ("object was modified"). If the optimistic lock is removed the
// stale patch merges silently, no conflict is raised, no retry happens, and the
// concurrent writer's namespace (team-c) is clobbered — failing this test.
func TestGateway_EnsureRetriesOnRealOptimisticLockConflict(t *testing.T) {
	scheme := newTestScheme()
	// Gateway starts allowing gateway-ns and team-a.
	gw := gwWithNamespaceSelector("my-gateway", "gateway-ns", "gateway-ns", "team-a")
	md := newModelDeployment("test-model", "team-b")

	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	detector.PatchGateway = true

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&airunwayv1alpha1.ModelDeployment{}).
		WithObjects(md, gw).
		Build()

	var getCalls, patchCalls int
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*gatewayv1.Gateway); !ok {
				return nil
			}
			getCalls++
			if getCalls == 1 {
				// A concurrent writer lands AFTER our helper's first read but
				// BEFORE its patch: add team-c and let the fake client bump the
				// stored resourceVersion. Our helper still holds the pre-bump
				// object, so its optimistic-lock patch will be stale -> real 409.
				var stored gatewayv1.Gateway
				if err := base.Get(ctx, key, &stored); err != nil {
					return err
				}
				fromSelector := gatewayv1.NamespacesFromSelector
				storedBase := stored.DeepCopy()
				for i := range stored.Spec.Listeners {
					stored.Spec.Listeners[i].AllowedRoutes = &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: &fromSelector,
							Selector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      "kubernetes.io/metadata.name",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"gateway-ns", "team-a", "team-c"},
								}},
							},
						},
					}
				}
				if err := base.Patch(ctx, &stored, client.MergeFrom(storedBase)); err != nil {
					return err
				}
			}
			return nil
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*gatewayv1.Gateway); ok {
				patchCalls++
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})

	r := &ModelDeploymentReconciler{
		Client:           c,
		Scheme:           scheme,
		GatewayDetector:  detector,
		ProviderResolver: gateway.NewInferenceProviderConfigResolver(c),
	}
	ctx := context.Background()

	gwConfig := &gateway.GatewayConfig{GatewayName: "my-gateway", GatewayNamespace: "gateway-ns"}
	if err := r.ensureGatewayAllowsNamespace(ctx, gwConfig, "team-b"); err != nil {
		t.Fatalf("ensureGatewayAllowsNamespace failed: %v", err)
	}

	// The helper must have retried: >=2 gets (initial + post-conflict re-read) and
	// >=2 patch attempts (the stale one that 409'd + the successful retry).
	if getCalls < 2 {
		t.Errorf("expected at least 2 gets (initial + post-conflict re-read), got %d", getCalls)
	}
	if patchCalls < 2 {
		t.Errorf("expected at least 2 patch attempts (1 real 409 + 1 success), got %d", patchCalls)
	}

	// team-c (concurrent) AND team-b (ours) both survive: proof the retry re-read
	// fresh state and unioned rather than clobbering.
	var finalGW gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Name: "my-gateway", Namespace: "gateway-ns"}, &finalGW); err != nil {
		t.Fatalf("failed to get Gateway: %v", err)
	}
	for _, l := range finalGW.Spec.Listeners {
		sel := l.AllowedRoutes.Namespaces.Selector
		if sel == nil || len(sel.MatchExpressions) == 0 {
			t.Fatal("expected matchExpressions to be set")
		}
		values := sel.MatchExpressions[0].Values
		want := []string{"gateway-ns", "team-a", "team-b", "team-c"}
		if len(values) != len(want) {
			t.Fatalf("expected %v, got %v", want, values)
		}
		for i := range want {
			if values[i] != want[i] {
				t.Errorf("expected %v, got %v", want, values)
				break
			}
		}
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
		// dynamo-system is removed, kaito-workspace remains, and gateway-ns is
		// re-seeded into the Selector by the shared terminal rule — so a cleanup
		// that still leaves cross-namespace routes self-heals the Gateway's own
		// namespace into the In-list too (#333).
		if len(values) != 2 || values[0] != "gateway-ns" || values[1] != "kaito-workspace" {
			t.Errorf("expected [gateway-ns, kaito-workspace] in selector values, got %v", values)
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
		// gateway-ns is re-seeded into every Selector we write, so converting
		// Same->Selector (or extending an existing Selector) never drops the
		// Gateway's own namespace and evicts its routes (#333). It also self-heals
		// this Gateway, which started without gateway-ns in the Selector.
		if len(values) != 3 {
			t.Fatalf("expected 3 namespaces in selector, got %v", values)
		}
		// Values are sorted
		if values[0] != "dynamo-system" || values[1] != "gateway-ns" || values[2] != "kaito-workspace" {
			t.Errorf("expected [dynamo-system, gateway-ns, kaito-workspace], got %v", values)
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
		// gateway-ns is re-seeded so migrating legacy matchLabels to a
		// matchExpressions Selector never drops the Gateway's own namespace (#333).
		if len(values) != 3 {
			t.Fatalf("expected 3 namespaces after migration, got %v", values)
		}
		if values[0] != "dynamo-system" || values[1] != "gateway-ns" || values[2] != "kaito-workspace" {
			t.Errorf("expected [dynamo-system, gateway-ns, kaito-workspace], got %v", values)
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

// TestGateway_EPP_DefaultsWhenNoOverrides verifies that when a provider does
// not declare EndpointPicker capabilities, the controller falls back to the
// built-in GAIE EPP image and a minimal default plugin ConfigMap.
func TestGateway_EPP_DefaultsWhenNoOverrides(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	if err := r.reconcileEPP(ctx, md, nil); err != nil {
		t.Fatalf("reconcileEPP failed: %v", err)
	}

	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model-epp", Namespace: "default"}, &dep); err != nil {
		t.Fatalf("EPP Deployment not found: %v", err)
	}
	img := dep.Spec.Template.Spec.Containers[0].Image
	if !strings.HasPrefix(img, "registry.k8s.io/gateway-api-inference-extension/epp:") {
		t.Errorf("expected default GAIE EPP image, got %q", img)
	}

	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model-epp", Namespace: "default"}, &cm); err != nil {
		t.Fatalf("EPP ConfigMap not found: %v", err)
	}
	if !strings.Contains(cm.Data["default-plugins.yaml"], "EndpointPickerConfig") {
		t.Errorf("expected default EndpointPickerConfig ConfigMap, got %q", cm.Data["default-plugins.yaml"])
	}
}

// TestGateway_EPP_ProviderOverrides verifies that when a provider supplies
// EndpointPicker capabilities the controller-managed EPP Deployment uses the
// provider's image and the ConfigMap carries the provider's plugin config.
func TestGateway_EPP_ProviderOverrides(t *testing.T) {
	scheme := newTestScheme()
	md := newModelDeployment("test-model", "default")
	detector := fakeDetector(true, "my-gateway", "gateway-ns")
	r := newTestReconciler(scheme, detector, md)
	ctx := context.Background()

	overrides := &airunwayv1alpha1.EndpointPickerCapabilities{
		Image:      "ghcr.io/example/custom-epp:v1.2.3",
		ConfigData: "apiVersion: llm-d.ai/v1alpha1\nkind: EndpointPickerConfig\nplugins:\n- type: custom-scorer\n",
	}
	if err := r.reconcileEPP(ctx, md, overrides); err != nil {
		t.Fatalf("reconcileEPP failed: %v", err)
	}

	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model-epp", Namespace: "default"}, &dep); err != nil {
		t.Fatalf("EPP Deployment not found: %v", err)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != overrides.Image {
		t.Errorf("expected EPP image %q, got %q", overrides.Image, got)
	}

	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: "test-model-epp", Namespace: "default"}, &cm); err != nil {
		t.Fatalf("EPP ConfigMap not found: %v", err)
	}
	if got := cm.Data["default-plugins.yaml"]; got != overrides.ConfigData {
		t.Errorf("expected EPP ConfigMap data to match provider overrides, got %q", got)
	}
}

// TestGateway_EPP_OnlyImageOverride verifies image-only overrides leave the
// default ConfigMap in place, and that the inverse (config-only via empty
// Image) leaves the default GAIE EPP image in place.
func TestGateway_EPP_OnlyImageOverride(t *testing.T) {
	t.Run("image only keeps default ConfigMap", func(t *testing.T) {
		scheme := newTestScheme()
		md := newModelDeployment("test-model", "default")
		detector := fakeDetector(true, "my-gateway", "gateway-ns")
		r := newTestReconciler(scheme, detector, md)
		ctx := context.Background()

		if err := r.reconcileEPP(ctx, md, &airunwayv1alpha1.EndpointPickerCapabilities{Image: "ghcr.io/example/only:v0"}); err != nil {
			t.Fatalf("reconcileEPP failed: %v", err)
		}

		var dep appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: "test-model-epp", Namespace: "default"}, &dep); err != nil {
			t.Fatalf("EPP Deployment not found: %v", err)
		}
		if got := dep.Spec.Template.Spec.Containers[0].Image; got != "ghcr.io/example/only:v0" {
			t.Errorf("expected overridden EPP image, got %q", got)
		}

		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: "test-model-epp", Namespace: "default"}, &cm); err != nil {
			t.Fatalf("EPP ConfigMap not found: %v", err)
		}
		if !strings.Contains(cm.Data["default-plugins.yaml"], "EndpointPickerConfig") {
			t.Errorf("expected default EndpointPickerConfig ConfigMap when only image is overridden, got %q", cm.Data["default-plugins.yaml"])
		}
	})

	t.Run("config only keeps default image", func(t *testing.T) {
		scheme := newTestScheme()
		md := newModelDeployment("test-model", "default")
		detector := fakeDetector(true, "my-gateway", "gateway-ns")
		r := newTestReconciler(scheme, detector, md)
		ctx := context.Background()

		configData := "apiVersion: llm-d.ai/v1alpha1\nkind: EndpointPickerConfig\nplugins:\n- type: custom-scorer\n"
		if err := r.reconcileEPP(ctx, md, &airunwayv1alpha1.EndpointPickerCapabilities{ConfigData: configData}); err != nil {
			t.Fatalf("reconcileEPP failed: %v", err)
		}

		var dep appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: "test-model-epp", Namespace: "default"}, &dep); err != nil {
			t.Fatalf("EPP Deployment not found: %v", err)
		}
		if img := dep.Spec.Template.Spec.Containers[0].Image; !strings.HasPrefix(img, "registry.k8s.io/gateway-api-inference-extension/epp:") {
			t.Errorf("expected default GAIE EPP image when only config is overridden, got %q", img)
		}

		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: "test-model-epp", Namespace: "default"}, &cm); err != nil {
			t.Fatalf("EPP ConfigMap not found: %v", err)
		}
		if got := cm.Data["default-plugins.yaml"]; got != configData {
			t.Errorf("expected EPP ConfigMap data to match provider override, got %q", got)
		}
	})
}
