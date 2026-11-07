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
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

const gatewayFinalLabelAttempt = "controller final attempt"

func (e *gatewayPoolRefAPIEnv) testEnabledRouteCollision(t *testing.T, ownership string) {
	f := newGatewayPoolRefAPIFixture(t, e.client)
	f.usePool(t, false)
	port := gatewayv1.PortNumber(8080)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: f.md.Name, Namespace: f.md.Namespace},
		Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{{
			BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "foreign-service", Port: &port},
			}}},
		}}},
	}
	if ownership == "other controller" {
		other := f.md.DeepCopy()
		other.ObjectMeta = metav1.ObjectMeta{Name: "other-model", Namespace: f.md.Namespace}
		if err := e.client.Create(t.Context(), other); err != nil {
			t.Fatal(err)
		}
		if err := ctrl.SetControllerReference(other, route, e.scheme); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.client.Create(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	unchanged := f.snapshot(t, route, f.pool, f.epp, f.eppService, f.pod, f.userPod)
	r := newGatewayAPIReconciler(t, e.config, e.client, e.scheme, f.md.Namespace)
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.md)})
	if err == nil || !strings.Contains(err.Error(), "not controlled by") {
		t.Errorf("foreign route did not report an ownership collision: %v", err)
	}
	f.assertUnchanged(t, unchanged)
	f.assertGatewayCondition(t, metav1.ConditionFalse, "HTTPRouteFailed")
	// The foreign owner releases the name. A fresh controller can create its
	// own route; the previous collision must not mark that route as created.
	if err := e.client.Delete(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, newGatewayAPIReconciler(t, e.config, e.client, e.scheme, f.md.Namespace))
	f.assertRoute(t)
	f.get(t, client.ObjectKeyFromObject(f.md), route)
	if !metav1.IsControlledBy(route, f.md) {
		t.Error("recovered route is not owned by the actual ModelDeployment")
	}
	f.assertGatewayCondition(t, metav1.ConditionTrue, "GatewayConfigured")
}

func (e *gatewayPoolRefAPIEnv) testManagedPodConflict(t *testing.T, mode string) {
	f := newGatewayPoolRefAPIFixture(t, e.client)
	providerManaged := mode == "provider managed"
	if providerManaged {
		f.useProviderManagedPool(t)
	}
	unchanged := f.snapshot(t, f.userPod, f.pool, f.epp, f.eppService)
	fault := &gatewayPodLabelAPIFault{fixture: f, afterFirstWrite: mode == gatewayFinalLabelAttempt}
	c := interceptor.NewClient(e.client, interceptor.Funcs{Patch: fault.patch})
	r := newGatewayAPIReconciler(t, e.config, c, e.scheme, f.md.Namespace)
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.md)})
	if fault.conflicts == 0 {
		f.get(t, client.ObjectKeyFromObject(f.md), f.md)
		t.Fatalf("the API did not reject the stale pod patch: reconcile=%v, status=%#v", err, f.md.Status)
	}
	if mode == gatewayFinalLabelAttempt && !fault.firstWriteObserved {
		t.Error("final-attempt conflict did not follow an actual initial controller label write")
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("pod conflict was swallowed instead of requesting retry: %v", err)
	}
	f.assertLabel(t, f.pod, "")
	f.assertGatewayCondition(t, metav1.ConditionFalse, "PodLabelConflict")
	f.reconcile(t, newGatewayAPIReconciler(t, e.config, e.client, e.scheme, f.md.Namespace))
	f.assertLabel(t, f.pod, f.md.Name)
	f.get(t, client.ObjectKeyFromObject(f.pod), f.pod)
	if f.pod.Labels["external-write"] != strconv.Itoa(fault.conflicts) {
		t.Error("retry overwrote the concurrent writer's unrelated label")
	}
	if f.pod.Labels[gatewayPodLabelOwner] != string(f.md.UID) {
		t.Error("retry did not record actual controller-written label provenance")
	}
	f.assertUnchanged(t, unchanged)
	f.assertGatewayCondition(t, metav1.ConditionTrue, "GatewayConfigured")
	if providerManaged {
		f.assertRoute(t)
		f.assertManagedResourcesGone(t)
	}
	unchanged = append(unchanged, f.snapshot(t, f.pod)...)
	conflictsBeforeNoOp := fault.conflicts
	f.reconcile(t, r)
	f.assertUnchanged(t, unchanged)
	if fault.conflicts != conflictsBeforeNoOp {
		t.Errorf("stable labeled pod was patched again: %d conflicts", fault.conflicts)
	}
}

func (f *gatewayPoolRefAPIFixture) useProviderManagedPool(t *testing.T) {
	t.Helper()
	provider := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: f.md.Namespace},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			Capabilities: &airunwayv1alpha1.ProviderCapabilities{
				Engines: []airunwayv1alpha1.EngineCapability{{
					Name:         airunwayv1alpha1.EngineTypeVLLM,
					ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated},
					GPUSupport:   true,
					Gateway: &airunwayv1alpha1.GatewayCapabilities{
						ManagesInferencePool: true, InferencePoolNamePattern: f.pool.Name,
					},
				}},
			},
		},
	}
	if err := f.client.Create(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.IgnoreNotFound(f.client.Delete(context.Background(), provider)); err != nil {
			t.Errorf("remove fixture provider: %v", err)
		}
	})
	f.md.Spec.Provider.Name = provider.Name
	if err := f.client.Update(t.Context(), f.md); err != nil {
		t.Fatal(err)
	}
	f.md.Status.Provider.Name = provider.Name
	if err := f.client.Status().Update(t.Context(), f.md); err != nil {
		t.Fatal(err)
	}
}

type gatewayPodLabelAPIFault struct {
	fixture            *gatewayPoolRefAPIFixture
	conflicts          int
	afterFirstWrite    bool
	firstWriteObserved bool
}

func (fault *gatewayPodLabelAPIFault) patch(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	f := fault.fixture
	if _, isPod := obj.(*corev1.Pod); isPod && obj.GetName() == f.pod.Name {
		if fault.afterFirstWrite {
			fault.afterFirstWrite = false
			return fault.finishFirstWrite(ctx, c, obj, patch, opts...)
		}
		current := &corev1.Pod{}
		if err := f.client.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return err
		}
		current.Labels["external-write"] = strconv.Itoa(fault.conflicts + 1)
		if err := f.client.Update(ctx, current); err != nil {
			return err
		}
	}
	err := c.Patch(ctx, obj, patch, opts...)
	if apierrors.IsConflict(err) {
		fault.conflicts++
	}
	return err
}

// finishFirstWrite permits the initial production patch, then models another
// writer removing its selector label. The default-pool helper must label it
// again; the next patch then gets a genuine API conflict at that final hop.
func (fault *gatewayPodLabelAPIFault) finishFirstWrite(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if err := c.Patch(ctx, obj, patch, opts...); err != nil {
		return err
	}
	f := fault.fixture
	current := &corev1.Pod{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
		return err
	}
	fault.firstWriteObserved = current.Labels[airunwayv1alpha1.LabelModelDeployment] == f.md.Name &&
		current.Labels[gatewayPodLabelOwner] == string(f.md.UID)
	delete(current.Labels, airunwayv1alpha1.LabelModelDeployment)
	return f.client.Update(ctx, current)
}

func (f *gatewayPoolRefAPIFixture) assertGatewayCondition(t *testing.T, status metav1.ConditionStatus, reason string) {
	t.Helper()
	current := &airunwayv1alpha1.ModelDeployment{}
	f.get(t, client.ObjectKeyFromObject(f.md), current)
	condition := meta.FindStatusCondition(current.Status.Conditions, airunwayv1alpha1.ConditionTypeGatewayReady)
	if condition == nil || condition.Status != status || condition.Reason != reason {
		t.Errorf("GatewayReady = %#v, want %s/%s", condition, status, reason)
	}
}
