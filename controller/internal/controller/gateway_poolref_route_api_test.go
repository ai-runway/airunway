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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

const (
	gatewayRouteCreationMarker = "true"
	gatewayRouteDeleting       = "deleting"
)

func (e *gatewayPoolRefAPIEnv) managedRouteReference(t *testing.T) (*gatewayPoolRefAPIFixture, *gatewayv1.HTTPRoute) {
	t.Helper()
	f := newGatewayPoolRefAPIFixture(t, e.client)
	f.reconcile(t, newGatewayAPIReconciler(t, e.config, e.client, e.scheme, f.md.Namespace))
	route := &gatewayv1.HTTPRoute{}
	f.get(t, client.ObjectKeyFromObject(f.md), route)
	f.get(t, client.ObjectKeyFromObject(f.md), f.md)
	// Ownership, the old backend and the creation marker must come from the
	// production controller, not a fixture pretending it created a route.
	if !metav1.IsControlledBy(route, f.md) || f.md.Annotations[airunwayv1alpha1.HTTPRouteCreated] != gatewayRouteCreationMarker {
		t.Fatal("initial reconciliation did not establish managed route ownership and its creation marker")
	}
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 || string(route.Spec.Rules[0].BackendRefs[0].Name) != f.md.Name {
		t.Fatalf("initial route does not target the managed pool: %#v", route.Spec.Rules)
	}
	f.md.Spec.Gateway.PoolRef = f.pool.Name
	f.md.Spec.Gateway.HTTPRouteRef = f.md.Name
	if err := e.client.Update(t.Context(), f.md); err != nil {
		t.Fatal(err)
	}
	return f, route
}

func (e *gatewayPoolRefAPIEnv) testManagedRouteReference(t *testing.T, mode string) {
	f, oldRoute := e.managedRouteReference(t)
	f.setRouteReferenceMode(t, mode)
	unchanged := f.snapshot(t, f.pool, f.epp, f.eppService, f.userPod)
	f.reconcile(t, newGatewayAPIReconciler(t, e.config, e.client, e.scheme, f.md.Namespace))
	f.assertRetiredRoute(t)
	f.assertManagedResourcesGone(t)
	f.assertLabel(t, f.pod, "")
	f.assertUnchanged(t, unchanged)
	// A fresh controller must neither resurrect the stale route nor modify
	// external resources while the explicit HTTPRoute reference remains set.
	f.reconcile(t, newGatewayAPIReconciler(t, e.config, e.client, e.scheme, f.md.Namespace))
	f.assertRetiredRoute(t)
	f.assertUnchanged(t, unchanged)
	if mode == "missing pool" {
		f.assertGatewayCondition(t, metav1.ConditionFalse, "InferencePoolNotFound")
	}
	if mode == gatewayRouteDeleting {
		return
	}
	// Clearing the explicit reference restores managed routing to the new
	// pool. A lingering creation marker would incorrectly suppress recovery.
	f.get(t, client.ObjectKeyFromObject(f.md), f.md)
	f.md.Spec.Gateway.HTTPRouteRef = ""
	f.md.Spec.Gateway.PoolRef = f.pool.Name
	f.md.Spec.Gateway.Enabled = boolPtr(true)
	if err := e.client.Update(t.Context(), f.md); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, newGatewayAPIReconciler(t, e.config, e.client, e.scheme, f.md.Namespace))
	f.assertRoute(t)
	recovered := &gatewayv1.HTTPRoute{}
	f.get(t, client.ObjectKeyFromObject(f.md), recovered)
	if recovered.UID == oldRoute.UID || !metav1.IsControlledBy(recovered, f.md) {
		t.Error("recovery did not create a new controller-owned route")
	}
	f.assertUnchanged(t, unchanged)
}

func (e *gatewayPoolRefAPIEnv) testForeignRouteReference(t *testing.T, ownership, mode string) {
	f := newGatewayPoolRefAPIFixture(t, e.client)
	f.md.Spec.Gateway.PoolRef = f.pool.Name
	f.md.Spec.Gateway.HTTPRouteRef = f.md.Name
	f.setRouteReferenceMode(t, mode)
	port := gatewayv1.PortNumber(8080)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: f.md.Name, Namespace: f.md.Namespace},
		Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{{
			BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "user-service", Port: &port},
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
	for range 2 {
		f.reconcile(t, newGatewayAPIReconciler(t, e.config, e.client, e.scheme, f.md.Namespace))
		f.assertUnchanged(t, unchanged)
	}
}

func (e *gatewayPoolRefAPIEnv) testRouteReferenceRetry(t *testing.T, failure string) {
	f, route := e.managedRouteReference(t)
	fault := &gatewayRouteRetirementAPIFault{changeOwner: failure == "owner change"}
	c := interceptor.NewClient(e.client, interceptor.Funcs{Delete: fault.delete})
	r := newGatewayAPIReconciler(t, e.config, c, e.scheme, f.md.Namespace)
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.md)})
	if !fault.injected {
		t.Fatal("retirement did not reach the managed HTTPRoute deletion")
	}
	if err == nil {
		t.Error("route deletion failure was swallowed instead of requesting retry")
	}
	if fault.changeOwner && !apierrors.IsConflict(err) {
		t.Errorf("API did not reject the stale route deletion: %v", err)
	}
	f.get(t, client.ObjectKeyFromObject(route), route)
	f.get(t, client.ObjectKeyFromObject(f.md), f.md)
	if f.md.Annotations[airunwayv1alpha1.HTTPRouteCreated] != "" {
		t.Error("creation marker was not cleared before the failing route deletion")
	}
	unchanged := f.snapshot(t, f.pool, f.epp, f.eppService, f.userPod)
	if fault.changeOwner {
		if len(route.OwnerReferences) != 0 || route.Annotations["external-owner"] != "keep" {
			t.Fatal("concurrent ownership change was not preserved")
		}
		unchanged = append(unchanged, f.snapshot(t, route)...)
	}
	// The marker is already clear. Cleanup must retry from fresh API state,
	// preserving a route whose owner changed between the first GET and DELETE.
	for range 2 {
		f.reconcile(t, newGatewayAPIReconciler(t, e.config, e.client, e.scheme, f.md.Namespace))
		f.assertUnchanged(t, unchanged)
		if !fault.changeOwner {
			f.assertRetiredRoute(t)
		}
	}
}

func (f *gatewayPoolRefAPIFixture) setRouteReferenceMode(t *testing.T, mode string) {
	t.Helper()
	if mode == "missing pool" {
		f.md.Spec.Gateway.PoolRef = "missing-pool"
	}
	f.md.Spec.Gateway.Enabled = boolPtr(mode != "disabled")
	if mode == gatewayRouteDeleting {
		f.md.Finalizers = append(f.md.Finalizers, "tests.airunway.ai/hold")
	}
	if err := f.client.Update(t.Context(), f.md); err != nil {
		t.Fatal(err)
	}
	if mode == gatewayRouteDeleting {
		if err := f.client.Delete(t.Context(), f.md); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *gatewayPoolRefAPIFixture) assertRetiredRoute(t *testing.T) {
	t.Helper()
	route := &gatewayv1.HTTPRoute{}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.md), route); !apierrors.IsNotFound(err) {
		t.Errorf("stale controller-owned route survived the explicit reference transition: %v; rules=%#v", err, route.Spec.Rules)
	}
	f.get(t, client.ObjectKeyFromObject(f.md), f.md)
	if f.md.Annotations[airunwayv1alpha1.HTTPRouteCreated] != "" {
		t.Error("retirement left the HTTPRoute creation marker set")
	}
}

type gatewayRouteRetirementAPIFault struct {
	changeOwner bool
	injected    bool
}

func (fault *gatewayRouteRetirementAPIFault) delete(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
	if _, isRoute := obj.(*gatewayv1.HTTPRoute); isRoute && !fault.injected {
		fault.injected = true
		if !fault.changeOwner {
			return apierrors.NewServiceUnavailable("injected route deletion failure")
		}
		current := &gatewayv1.HTTPRoute{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return err
		}
		current.OwnerReferences = nil
		current.Annotations = map[string]string{"external-owner": "keep"}
		if err := c.Update(ctx, current); err != nil {
			return err
		}
	}
	return c.Delete(ctx, obj, opts...)
}
