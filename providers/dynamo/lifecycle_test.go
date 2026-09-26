package dynamo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func requestFixture(t *testing.T, md *api.ModelDeployment, phase string) *unstructured.Unstructured {
	t.Helper()
	objects, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	request := objects[0]
	request.SetUID("request-uid")
	request.Object["status"] = map[string]any{"phase": phase}
	hash, err := dynamointent.Fingerprint(md)
	if err != nil {
		t.Fatal(err)
	}
	a := request.GetAnnotations()
	a[dynamointent.HashAnnotation] = hash
	a[dynamointent.AttemptAnnotation] = ""
	request.SetAnnotations(a)
	return request
}

func workloadFixture(request *unstructured.Unstructured, version string) *unstructured.Unstructured {
	dgd := newDynamoResource(version, DynamoGraphDeploymentKind, "custom-serving-name", request.GetNamespace())
	dgd.SetUID("workload-uid")
	dgd.SetLabels(map[string]string{dynamoDGDRNameLabel: request.GetName(), dynamoDGDRNamespaceLabel: request.GetNamespace()})
	_ = unstructured.SetNestedField(request.Object, dgd.GetName(), "status", "dgdName")
	return dgd
}

func TestIntentLockedInputsNeverDelete(t *testing.T) {
	for _, phase := range []string{"Profiling", "Ready", "Deploying", "Deployed"} {
		t.Run(phase, func(t *testing.T) {
			md := newMDForController("test", "default")
			setIntentMode(md)
			request := requestFixture(t, md, phase)
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(request).Build()
			r := NewDynamoProviderReconciler(c, newScheme(), "")
			md.Spec.Model.ID = "different/model"
			desired, err := r.Transformer.Transform(context.Background(), md)
			if err != nil {
				t.Fatal(err)
			}
			err = r.createOrUpdateResource(context.Background(), desired[0], md)
			var locked *intentLockedError
			if !errors.As(err, &locked) {
				t.Fatalf("want locked error, got %v", err)
			}
			current := request.DeepCopy()
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(current), current); err != nil {
				t.Fatal(err)
			}
			if current.GetUID() != request.GetUID() || current.GetResourceVersion() != request.GetResourceVersion() {
				t.Fatal("locked request was mutated")
			}
		})
	}
}

func TestPendingInputEditsAndDefaultedMetadata(t *testing.T) {
	for _, phase := range []string{"", "Pending", "Failed"} {
		t.Run(phase, func(t *testing.T) {
			md := newMDForController("test", "default")
			setIntentMode(md)
			request := requestFixture(t, md, phase)
			a := request.GetAnnotations()
			a["operator/default"] = "keep"
			request.SetAnnotations(a)
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(request).Build()
			r := NewDynamoProviderReconciler(c, newScheme(), "")
			md.Spec.Resources.GPU.Count = 2
			desired, err := r.Transformer.Transform(context.Background(), md)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.createOrUpdateResource(context.Background(), desired[0], md); err != nil {
				t.Fatal(err)
			}
			got := request.DeepCopy()
			_ = c.Get(context.Background(), client.ObjectKeyFromObject(request), got)
			budget, _, _ := unstructured.NestedInt64(got.Object, "spec", "hardware", "totalGpus")
			if budget != 2 || got.GetAnnotations()["operator/default"] != "keep" {
				t.Fatalf("bad pending update: %#v", got.Object)
			}
			// Operator-discovered values do not cause a write on the following reconcile.
			_ = unstructured.SetNestedField(got.Object, "H100", "spec", "hardware", "gpuSku")
			if err := c.Update(context.Background(), got); err != nil {
				t.Fatal(err)
			}
			before := got.GetResourceVersion()
			if err := r.createOrUpdateResource(context.Background(), desired[0], md); err != nil {
				t.Fatal(err)
			}
			_ = c.Get(context.Background(), client.ObjectKeyFromObject(request), got)
			if got.GetResourceVersion() != before {
				t.Fatal("unchanged inputs overwrote operator defaults")
			}
		})
	}
}

func TestLegacyRequestMigrationAndGatewayEdit(t *testing.T) {
	md := newMDForController("test", "default")
	setIntentMode(md)
	request := requestFixture(t, md, "Deployed")
	a := request.GetAnnotations()
	delete(a, dynamointent.HashAnnotation)
	request.SetAnnotations(a)
	_ = unstructured.SetNestedField(request.Object, "H100", "spec", "hardware", "gpuSku")
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(request).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	md.Generation = 100
	md.Spec.Gateway = &api.GatewaySpec{Enabled: boolPtr(false)}
	desired, err := r.Transformer.Transform(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.createOrUpdateResource(context.Background(), desired[0], md); err != nil {
		t.Fatal(err)
	}
	if desired[0].GetName() != md.Name || md.Status.Provider.RequestRef.UID != "request-uid" {
		t.Fatal("migration recreated the legacy request")
	}
	got := request.DeepCopy()
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(got), got)
	sku, _, _ := unstructured.NestedString(got.Object, "spec", "hardware", "gpuSku")
	if sku != "H100" {
		t.Fatal("migration overwrote discovered hardware")
	}
}

func TestExplicitAttemptCheckpointsBeforeCleanup(t *testing.T) {
	ctx := context.Background()
	md := newMDForController("test", "default")
	setIntentMode(md)
	md.Finalizers = []string{FinalizerName}
	request := requestFixture(t, md, "Deployed")
	dgd := workloadFixture(request, dynamoBetaVersion)
	md.Annotations = map[string]string{dynamointent.AttemptAnnotation: "retry-2"}
	md.Status.Phase = api.DeploymentPhaseRunning
	md.Status.Endpoint = &api.EndpointStatus{Service: "old-frontend"}
	var deleted []string
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(operatorRuntimeFixtures("1.1.1", "")...).WithObjects(md, request, dgd).WithStatusSubresource(md).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentRequestKind {
				obj.SetUID("new-request-uid")
			}
			return c.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			var current api.ModelDeployment
			if err := c.Get(ctx, client.ObjectKeyFromObject(md), &current); err != nil {
				return err
			}
			if current.Status.Provider.Intent == nil || current.Status.Provider.Intent.Phase != "Replacing" || current.Status.Endpoint != nil || current.Status.Provider.WorkloadRef == nil || current.Status.Provider.WorkloadRef.UID != "workload-uid" {
				t.Fatal("destructive operation before identity/status checkpoint")
			}
			options := &client.DeleteOptions{}
			options.ApplyOptions(opts)
			if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != obj.GetUID() {
				t.Fatal("missing UID delete precondition")
			}
			deleted = append(deleted, obj.GetObjectKind().GroupVersionKind().Kind)
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	for i := 0; i < 5; i++ {
		// A fresh reconciler each time models process restarts between checkpoints.
		r := NewDynamoProviderReconciler(c, newScheme(), "")
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(md)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(deleted) != 2 || deleted[0] != DynamoGraphDeploymentKind || deleted[1] != DynamoGraphDeploymentRequestKind {
		t.Fatalf("unexpected cleanup sequence %v", deleted)
	}
	var got api.ModelDeployment
	_ = c.Get(ctx, client.ObjectKeyFromObject(md), &got)
	if got.Status.Provider.RequestRef == nil || got.Status.Provider.RequestRef.Name != intentAttemptName(md) || got.Status.Provider.Intent.Attempt != "retry-2" {
		t.Fatalf("attempt not persisted: %#v", got.Status.Provider)
	}
}

func TestRequestCreateRecoveryDoesNotRepeatAttempt(t *testing.T) {
	md := newMDForController("test", "default")
	setIntentMode(md)
	md.Annotations = map[string]string{dynamointent.AttemptAnnotation: "2"}
	request := requestFixture(t, md, "Profiling")
	request.SetName(intentAttemptName(md))
	a := request.GetAnnotations()
	a[dynamointent.AttemptAnnotation] = "2"
	request.SetAnnotations(a)
	md.Status.Provider.RequestRef = resourceReference(newDynamoResource(DynamoGraphDeploymentRequestAPIVersion, DynamoGraphDeploymentRequestKind, "previous-request", md.Namespace))
	md.Status.Provider.Intent = &api.ProviderIntentStatus{Phase: "Replacing", Attempt: "1"}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(request).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	desired, err := r.Transformer.Transform(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.createOrUpdateResource(context.Background(), desired[0], md); err != nil {
		t.Fatal(err)
	}
	if md.Status.Provider.RequestRef.Name != request.GetName() || md.Status.Provider.Intent.Attempt != "2" {
		t.Fatal("did not recover new request")
	}
}

func TestGeneratedWorkloadUIDMismatchBlocksCleanup(t *testing.T) {
	md := newMDForController("test", "default")
	request := newDynamoResource(DynamoGraphDeploymentRequestAPIVersion, DynamoGraphDeploymentRequestKind, md.Name, md.Namespace)
	dgd := workloadFixture(request, dynamoBetaVersion)
	md.Status.Provider.WorkloadRef = resourceReference(dgd)
	dgd.SetUID("replacement-uid")
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(dgd).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	if _, err := r.deleteGeneratedDGDs(context.Background(), md, nil); err == nil {
		t.Fatal("UID mismatch was accepted")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dgd), dgd); err != nil {
		t.Fatal("replacement was deleted")
	}
}

func TestGeneratedServingStatusAndPoolResolution(t *testing.T) {
	for _, version := range []string{DynamoAPIVersion, dynamoBetaVersion} {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			md := newMDForController("test", "default")
			setIntentMode(md)
			request := requestFixture(t, md, "Deployed")
			dgd := workloadFixture(request, version)
			componentKey := "services"
			if version == dynamoBetaVersion {
				componentKey = "components"
			}
			dgd.Object["spec"] = map[string]any{componentKey: map[string]any{"CustomFront": map[string]any{"componentType": "frontend"}}}
			if version == dynamoBetaVersion {
				dgd.Object["spec"] = map[string]any{"components": []any{map[string]any{"name": "CustomFront", "type": "frontend"}}}
			}
			dgd.Object["status"] = map[string]any{"state": "successful", componentKey: map[string]any{"CustomFront": map[string]any{"replicas": int64(2), "availableReplicas": int64(1)}}}
			svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: dgd.GetName() + "-customfront", Namespace: md.Namespace, OwnerReferences: []metav1.OwnerReference{{UID: dgd.GetUID()}}}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8000}}}}
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(request, dgd, svc).Build()
			r := NewDynamoProviderReconciler(c, newScheme(), "")
			if err := r.syncStatus(ctx, md, request); err != nil {
				t.Fatal(err)
			}
			if md.Status.Phase == api.DeploymentPhaseRunning || md.Status.Replicas.Ready != 1 {
				t.Fatal("request completion hid degraded serving workload")
			}
			if md.Status.Endpoint == nil || md.Status.Endpoint.Service != svc.Name || md.Status.Provider.InferencePoolRef != nil {
				t.Fatalf("bad frontend binding: %#v", md.Status)
			}
			if md.Status.Provider.WorkloadRef.UID != "workload-uid" {
				t.Fatal("workload UID not captured")
			}
			_ = unstructured.SetNestedField(dgd.Object, int64(2), "status", componentKey, "CustomFront", "availableReplicas")
			_ = c.Update(ctx, dgd)
			if err := r.syncStatus(ctx, md, request); err != nil {
				t.Fatal(err)
			}
			if md.Status.Phase != api.DeploymentPhaseRunning {
				t.Fatal("healthy DGD not promoted")
			}
			_ = unstructured.SetNestedField(dgd.Object, "failed", "status", "state")
			_ = c.Update(ctx, dgd)
			if err := r.syncStatus(ctx, md, request); err != nil {
				t.Fatal(err)
			}
			if md.Status.Phase != api.DeploymentPhaseFailed {
				t.Fatal("workload failure hidden by Deployed request")
			}
		})
	}
}

func TestOnlyObservedOwnedInferencePoolIsPublished(t *testing.T) {
	md := newMDForController("test", "default")
	dgd := newDynamoResource(dynamoBetaVersion, DynamoGraphDeploymentKind, md.Name, md.Namespace)
	dgd.SetUID("dgd-uid")
	dgd.Object["spec"] = map[string]any{"components": []any{map[string]any{"name": "Epp", "type": "epp"}}}
	pool := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "inference.networking.k8s.io/v1", "kind": "InferencePool", "metadata": map[string]any{"name": md.Name + "-pool", "namespace": md.Namespace}, "spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "worker"}}}}}
	pool.SetUID("pool-uid")
	c := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	result := &ProviderStatusResult{}
	if err := r.resolveServingResources(context.Background(), md, dgd, result); err != nil {
		t.Fatal(err)
	}
	if md.Status.Provider.InferencePoolRef != nil {
		t.Fatal("published nonexistent pool")
	}
	if err := c.Create(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if err := r.resolveServingResources(context.Background(), md, dgd, result); err != nil {
		t.Fatal(err)
	}
	if md.Status.Provider.InferencePoolRef != nil {
		t.Fatal("published unrelated pool")
	}
	pool.SetOwnerReferences([]metav1.OwnerReference{{UID: dgd.GetUID()}})
	_ = c.Update(context.Background(), pool)
	if err := r.resolveServingResources(context.Background(), md, dgd, result); err != nil {
		t.Fatal(err)
	}
	if md.Status.Provider.InferencePoolRef == nil || md.Status.Provider.InferencePoolRef.UID != "pool-uid" {
		t.Fatal("owned observed pool missing")
	}
	_ = c.Delete(context.Background(), pool)
	if err := r.resolveServingResources(context.Background(), md, dgd, result); err != nil {
		t.Fatal(err)
	}
	if md.Status.Provider.InferencePoolRef != nil {
		t.Fatal("stale deleted pool reference")
	}
}

func TestGeneratedWorkloadWatchUsesRequestLabels(t *testing.T) {
	md := newMDForController("test", "default")
	setIntentMode(md)
	request := requestFixture(t, md, "Deployed")
	request.SetName("attempt-specific-name")
	dgd := workloadFixture(request, dynamoBetaVersion)
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(request).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	requests := r.mapDynamoWorkload(context.Background(), dgd)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(md) {
		t.Fatalf("wrong reconcile target %v", requests)
	}
}

func TestMissingRequestCannotSilentlyRetry(t *testing.T) {
	md := newMDForController("test", "default")
	setIntentMode(md)
	request := requestFixture(t, md, "Failed")
	md.Status.Provider.RequestRef = resourceReference(request)
	md.Status.Provider.Intent = &api.ProviderIntentStatus{Attempt: ""}
	c := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	desired, err := r.Transformer.Transform(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	var locked *intentLockedError
	if err := r.createOrUpdateResource(context.Background(), desired[0], md); !errors.As(err, &locked) {
		t.Fatalf("expected explicit retry requirement: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: intentAttemptName(md), Namespace: md.Namespace}, request); !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected new request: %v", err)
	}
}

func TestAttemptAnnotationUpdateTriggersReconcile(t *testing.T) {
	old := newMDForController("test", "default")
	setIntentMode(old)
	next := old.DeepCopy()
	next.Annotations = map[string]string{dynamointent.AttemptAnnotation: "retry"}
	if next.Generation != old.Generation {
		t.Fatal("fixture must use metadata-only update")
	}
	filter := predicate.NewPredicateFuncs(dynamoProviderPredicate)
	if !filter.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: next}) {
		t.Fatal("attempt-only edit was filtered out")
	}
}

func TestFrontendServiceOwnedThroughDCD(t *testing.T) {
	md := newMDForController("test", "default")
	dgd := newDynamoResource(dynamoBetaVersion, DynamoGraphDeploymentKind, md.Name, md.Namespace)
	dgd.SetUID("graph-uid")
	dcd := newDynamoResource(dynamoBetaVersion, "DynamoComponentDeployment", "test-frontend", md.Namespace)
	dcd.SetUID("component-uid")
	dcd.SetOwnerReferences([]metav1.OwnerReference{{UID: dgd.GetUID()}})
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-frontend", Namespace: md.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: dcd.GetAPIVersion(), Kind: dcd.GetKind(), Name: dcd.GetName(), UID: dcd.GetUID()}}}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8000}}}}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(dcd, svc).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	result := &ProviderStatusResult{Endpoint: &api.EndpointStatus{Service: svc.Name, Port: 8000}}
	if err := r.resolveServingResources(context.Background(), md, dgd, result); err != nil {
		t.Fatal(err)
	}
	if result.Endpoint == nil {
		t.Fatal("DCD-owned frontend Service not resolved")
	}
	dcd.SetOwnerReferences([]metav1.OwnerReference{{UID: "other-graph"}})
	_ = c.Update(context.Background(), dcd)
	if err := r.resolveServingResources(context.Background(), md, dgd, result); err != nil {
		t.Fatal(err)
	}
	if result.Endpoint != nil {
		t.Fatal("cross-graph frontend accepted")
	}
}

func TestInvalidIntentBypassingAdmissionCannotDeleteWorkload(t *testing.T) {
	md := newMDForController("test", "default")
	setIntentMode(md)
	md.Finalizers = []string{FinalizerName}
	request := requestFixture(t, md, "Deployed")
	dgd := workloadFixture(request, dynamoBetaVersion)
	md.Spec.Resources = nil
	md.Spec.Provider.Overrides.Raw = []byte(`{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":65}}}`)
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(operatorRuntimeFixtures("1.1.1", "")...).WithObjects(md, request, dgd).WithStatusSubresource(md).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			t.Fatal("invalid edit initiated teardown")
			return nil
		},
	}).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(md)}); err != nil {
		t.Fatal(err)
	}
	var got api.ModelDeployment
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(md), &got)
	if got.Status.Phase != api.DeploymentPhaseFailed {
		t.Fatal("invalid budget accepted")
	}
}

func TestDirectManualStatusPublishesActualPool(t *testing.T) {
	md := newMDForController("test", "default")
	dgd := newDynamoResource(dynamoBetaVersion, DynamoGraphDeploymentKind, md.Name, md.Namespace)
	dgd.SetUID("manual-workload")
	dgd.SetOwnerReferences([]metav1.OwnerReference{{UID: md.UID}})
	dgd.Object["spec"] = map[string]any{"components": []any{map[string]any{"name": "routing", "type": "epp"}}}
	dgd.Object["status"] = map[string]any{"state": "successful"}
	pool := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "inference.networking.k8s.io/v1", "kind": "InferencePool", "metadata": map[string]any{"name": md.Name + "-pool", "namespace": md.Namespace}, "spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "worker"}}}}}
	pool.SetUID("routing-pool")
	pool.SetOwnerReferences([]metav1.OwnerReference{{UID: dgd.GetUID()}})
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(dgd, pool).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	if err := r.syncStatus(context.Background(), md, dgd); err != nil {
		t.Fatal(err)
	}
	if md.Status.Provider.RequestRef != nil || md.Status.Provider.WorkloadRef.UID != "manual-workload" || md.Status.Provider.InferencePoolRef == nil || md.Status.Provider.InferencePoolRef.UID != "routing-pool" {
		t.Fatalf("bad manual binding %#v", md.Status.Provider)
	}
}

func TestIntentRequestNameLeavesRoomForUpstreamNames(t *testing.T) {
	md := newMDForController(strings.Repeat("long-model-name.", 15)+"tail", "default")
	name := intentAttemptName(md)
	if len(name) > 22 || strings.Contains(name, ".") {
		t.Fatalf("unsafe request name %q", name)
	}
	for _, derived := range []string{"profile-" + name, name + "-dgd-frontend", name + "-dgd-prefillworker", name + "-dgd-decodeworker"} {
		if len(derived) > 63 {
			t.Fatalf("upstream name exceeds limit: %s", derived)
		}
	}
	for _, component := range []string{"VllmPrefillWorker", "VllmDecodeWorker", "SglangPrefillWorker", "SglangDecodeWorker", "TrtllmPrefillWorker", "TrtllmDecodeWorker"} {
		if len(name+"-dgd")+len(component) > 45 {
			t.Fatalf("profiler would reject request %q with component %q", name, component)
		}
	}
	old := name
	md.Annotations = map[string]string{dynamointent.AttemptAnnotation: "next"}
	if intentAttemptName(md) == old {
		t.Fatal("attempts collide")
	}
}

func TestPreviousLongAttemptIsRecoveredWithoutCreatingAnotherRequest(t *testing.T) {
	for _, staleReference := range []bool{false, true} {
		t.Run(fmt.Sprint(staleReference), func(t *testing.T) {
			md := newMDForController("live11-auto", "default")
			setIntentMode(md)
			md.Annotations = map[string]string{dynamointent.AttemptAnnotation: "current-attempt"}
			existing := requestFixture(t, md, "Profiling")
			existing.SetName(previousIntentAttemptName(md))
			annotations := existing.GetAnnotations()
			annotations[dynamointent.AttemptAnnotation] = "current-attempt"
			existing.SetAnnotations(annotations)
			if staleReference {
				md.Status.Provider.RequestRef = &api.ProviderResourceReference{APIVersion: "nvidia.com/v1beta1", Kind: DynamoGraphDeploymentRequestKind, Namespace: md.Namespace, Name: "removed-previous-request", UID: "removed-uid"}
			}
			creates := 0
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(existing).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				creates++
				return cl.Create(ctx, obj, opts...)
			}}).Build()
			r := NewDynamoProviderReconciler(c, newScheme(), "")
			desired, err := r.Transformer.Transform(context.Background(), md)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.reconcileIntent(context.Background(), desired[0], md); err != nil {
				t.Fatal(err)
			}
			if creates != 0 || md.Status.Provider.RequestRef.Name != existing.GetName() || md.Status.Provider.RequestRef.UID != string(existing.GetUID()) {
				t.Fatalf("previous request was replaced: creates=%d ref=%+v", creates, md.Status.Provider.RequestRef)
			}
		})
	}
}
