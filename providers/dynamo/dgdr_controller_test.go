package dynamo

import (
	"context"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newIntentMD() *airunwayv1alpha1.ModelDeployment {
	md := newMDForController("intent", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Name: ProviderName,
		Overrides: &runtime.RawExtension{
			Raw: []byte(`{"deploymentMode":"intent"}`),
		},
	}
	return md
}

func ownedDynamoResource(
	version string,
	kind string,
	name string,
	md *airunwayv1alpha1.ModelDeployment,
) *unstructured.Unstructured {
	resource := newDynamoResource(version, kind)
	resource.SetName(name)
	resource.SetNamespace(md.Namespace)
	resource.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: airunwayv1alpha1.GroupVersion.String(),
			Kind:       "ModelDeployment",
			Name:       md.Name,
			UID:        md.UID,
		},
	})
	return resource
}

func TestEnsureIntentGatewayDisabled(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	md := newIntentMD()
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).Build()
	reconciler := NewDynamoProviderReconciler(client, scheme, "")

	var current airunwayv1alpha1.ModelDeployment
	key := types.NamespacedName{Name: md.Name, Namespace: md.Namespace}
	if err := client.Get(ctx, key, &current); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	changed, err := reconciler.ensureIntentGatewayDisabled(ctx, &current)
	if err != nil || !changed {
		t.Fatalf("ensureIntentGatewayDisabled() = %v, %v; want true, nil", changed, err)
	}
	if current.Spec.Gateway == nil || current.Spec.Gateway.Enabled == nil ||
		*current.Spec.Gateway.Enabled {
		t.Fatalf("gateway was not disabled: %#v", current.Spec.Gateway)
	}
	changed, err = reconciler.ensureIntentGatewayDisabled(ctx, &current)
	if err != nil || changed {
		t.Fatalf("second gateway check = %v, %v; want false, nil", changed, err)
	}
}

func TestReconcileIntentCreatesRequestAndStatus(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	md := newIntentMD()
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		Build()
	reconciler := NewDynamoProviderReconciler(client, scheme, "")

	var current airunwayv1alpha1.ModelDeployment
	key := types.NamespacedName{Name: md.Name, Namespace: md.Namespace}
	if err := client.Get(ctx, key, &current); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	result, err := reconciler.reconcileIntent(ctx, &current)
	if err != nil {
		t.Fatalf("reconcileIntent() error = %v", err)
	}
	if result.RequeueAfter != RequeueInterval {
		t.Errorf("requeueAfter = %v, want %v", result.RequeueAfter, RequeueInterval)
	}

	dgdr := newDynamoResource(
		DynamoGraphDeploymentRequestAPIVersion,
		DynamoGraphDeploymentRequestKind,
	)
	if err := client.Get(ctx, key, dgdr); err != nil {
		t.Fatalf("get created DGDR: %v", err)
	}
	if dgdr.GetAnnotations()[IntentHashAnnotation] == "" {
		t.Error("created DGDR has no intent hash")
	}
	if err := client.Get(ctx, key, &current); err != nil {
		t.Fatalf("get updated ModelDeployment: %v", err)
	}
	if current.Status.Provider.ResourceKind != DynamoGraphDeploymentRequestKind ||
		current.Status.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("unexpected ModelDeployment status: %#v", current.Status)
	}
}

func TestReconcileIntentReportsTransformFailure(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	md := newMDForController("direct", "default")
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		Build()
	reconciler := NewDynamoProviderReconciler(client, scheme, "")

	var current airunwayv1alpha1.ModelDeployment
	key := types.NamespacedName{Name: md.Name, Namespace: md.Namespace}
	if err := client.Get(ctx, key, &current); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	result, err := reconciler.reconcileIntent(ctx, &current)
	if err != nil {
		t.Fatalf("reconcileIntent() error = %v", err)
	}
	if result.RequeueAfter != ExternalRecoveryInterval {
		t.Errorf("requeueAfter = %v, want %v", result.RequeueAfter, ExternalRecoveryInterval)
	}
	if err := client.Get(ctx, key, &current); err != nil {
		t.Fatalf("get failed ModelDeployment: %v", err)
	}
	if current.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed ||
		current.Status.Endpoint != nil || current.Status.Replicas != nil {
		t.Errorf("unexpected failure status: %#v", current.Status)
	}
}

func TestEnsureIntentResourceRecreatesChangedIntent(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	md := newIntentMD()
	dgdr := ownedDynamoResource(
		DynamoGraphDeploymentRequestAPIVersion,
		DynamoGraphDeploymentRequestKind,
		md.Name,
		md,
	)
	dgdr.SetAnnotations(map[string]string{IntentHashAnnotation: "old"})
	dgdr.Object["status"] = map[string]any{"dgdName": "generated"}
	dgd := ownedDynamoResource(DynamoAPIVersion, DynamoGraphDeploymentKind, "generated", md)
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, dgdr, dgd).
		WithStatusSubresource(md).
		Build()
	reconciler := NewDynamoProviderReconciler(client, scheme, "")
	desired := dgdr.DeepCopy()
	desired.SetAnnotations(map[string]string{IntentHashAnnotation: "new"})

	var current airunwayv1alpha1.ModelDeployment
	key := types.NamespacedName{Name: md.Name, Namespace: md.Namespace}
	if err := client.Get(ctx, key, &current); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	result, handled, err := reconciler.ensureIntentResource(ctx, &current, desired)
	if err != nil || !handled || result.RequeueAfter == 0 {
		t.Fatalf("ensureIntentResource() = %#v, %v, %v", result, handled, err)
	}
	if err := client.Get(ctx, key, dgdr); !apierrors.IsNotFound(err) {
		t.Fatalf("DGDR still exists after recreation cleanup: %v", err)
	}
	dgdKey := types.NamespacedName{Name: "generated", Namespace: md.Namespace}
	if err := client.Get(ctx, dgdKey, dgd); !apierrors.IsNotFound(err) {
		t.Fatalf("generated DGD still exists after recreation cleanup: %v", err)
	}
	if current.Status.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("phase = %s, want Deploying", current.Status.Phase)
	}
}

func TestDeleteDirectDGDForIntent(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	md := newIntentMD()
	direct := ownedDynamoResource(DynamoAPIVersion, DynamoGraphDeploymentKind, md.Name, md)
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(direct).Build()
	reconciler := NewDynamoProviderReconciler(client, scheme, "")

	pending, err := reconciler.deleteDirectDGDForIntent(ctx, md)
	if err != nil || !pending {
		t.Fatalf("deleteDirectDGDForIntent() = %v, %v; want true, nil", pending, err)
	}
	key := types.NamespacedName{Name: md.Name, Namespace: md.Namespace}
	if err := client.Get(ctx, key, direct); !apierrors.IsNotFound(err) {
		t.Fatalf("direct DGD still exists: %v", err)
	}

	linked := ownedDynamoResource(DynamoAPIVersion, DynamoGraphDeploymentKind, md.Name, md)
	linked.SetLabels(map[string]string{dgdrNameLabel: md.Name})
	client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(linked).Build()
	reconciler = NewDynamoProviderReconciler(client, scheme, "")
	pending, err = reconciler.deleteDirectDGDForIntent(ctx, md)
	if err != nil || pending {
		t.Fatalf("linked DGD deletion = %v, %v; want false, nil", pending, err)
	}
}

func TestDeleteIntentResourcesRejectsUnlinkedDGD(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	md := newIntentMD()
	dgdr := ownedDynamoResource(
		DynamoGraphDeploymentRequestAPIVersion,
		DynamoGraphDeploymentRequestKind,
		md.Name,
		md,
	)
	dgdr.Object["status"] = map[string]any{"dgdName": "foreign"}
	foreign := newDynamoResource(DynamoAPIVersion, DynamoGraphDeploymentKind)
	foreign.SetName("foreign")
	foreign.SetNamespace(md.Namespace)
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dgdr, foreign).Build()
	reconciler := NewDynamoProviderReconciler(client, scheme, "")

	if _, err := reconciler.deleteIntentResources(ctx, md); err == nil {
		t.Fatal("expected conflict for a same-name DGD without verified links")
	}
}

func TestDGDLinksAndResourceMapping(t *testing.T) {
	md := newIntentMD()
	owned := ownedDynamoResource(DynamoAPIVersion, DynamoGraphDeploymentKind, "owned", md)
	if !isDGDLinkedToModelDeployment(owned, md) {
		t.Error("owner reference should link DGD")
	}

	managed := newDynamoResource(DynamoAPIVersion, DynamoGraphDeploymentKind)
	managed.SetLabels(map[string]string{
		airunwayv1alpha1.LabelManagedBy:       "airunway",
		airunwayv1alpha1.LabelModelDeployment: md.Name,
	})
	if !isDGDLinkedToModelDeployment(managed, md) {
		t.Error("managed labels should link DGD")
	}
	managed.SetLabels(map[string]string{
		dgdrNameLabel:      md.Name,
		dgdrNamespaceLabel: "other",
	})
	if isDGDLinkedToModelDeployment(managed, md) {
		t.Error("a mismatched DGDR namespace must not link DGD")
	}

	owned.SetNamespace(md.Namespace)
	requests := mapDynamoResourceToModelDeployment(context.Background(), owned)
	if len(requests) != 1 || requests[0].Name != md.Name {
		t.Fatalf("owner mapping = %#v", requests)
	}
	managed.SetLabels(map[string]string{
		dgdrNameLabel:      md.Name,
		dgdrNamespaceLabel: "linked-namespace",
	})
	requests = mapDynamoResourceToModelDeployment(context.Background(), managed)
	if len(requests) != 1 || requests[0].Namespace != "linked-namespace" {
		t.Fatalf("label mapping = %#v", requests)
	}
	managed.SetLabels(nil)
	if requests := mapDynamoResourceToModelDeployment(context.Background(), managed); requests != nil {
		t.Fatalf("unlinked mapping = %#v, want nil", requests)
	}
}
