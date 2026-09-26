package dynamo

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Existing lifecycle tests model a concrete 1.1.1 operator, not a runtime inferred
// from whichever API the fake mapper happens to serve.
func operatorRuntimeFixtures(version, namespace string) []client.Object {
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "operator-config-" + version, Namespace: "dynamo-system"}, Data: map[string]string{"config.yaml": "apiVersion: operator.config.dynamo.nvidia.com/v1alpha1\nkind: OperatorConfiguration\nnamespace:\n  restricted: \"" + namespace + "\"\n"}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "operator-" + version, Namespace: "dynamo-system", Generation: 1, Labels: map[string]string{"app.kubernetes.io/part-of": "dynamo-operator", "app.kubernetes.io/version": version}}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "manager", Image: "nvcr.io/nvidia/ai-dynamo/kubernetes-operator:" + version, Args: []string{"--operator-version=" + version}}},
		Volumes:    []corev1.Volume{{Name: "operator-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: config.Name}}}}},
	}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}}
	return []client.Object{deployment, config}
}

func TestRuntimeDiscoveryUsesOperatorNotServedAPIVersion(t *testing.T) {
	for _, version := range []string{"1.1.1", "1.5.0"} {
		t.Run(version, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(operatorRuntimeFixtures(version, "")...).WithRESTMapper(dynamoMapper(DynamoAPIVersion)).Build()
			r := NewDynamoProviderReconciler(c, newScheme(), "")
			actual, err := r.discoverRuntimeVersion(context.Background(), "default")
			if err != nil || actual != version {
				t.Fatalf("operator version %s: got %s (%v)", version, actual, err)
			}
			objects, err := r.renderResources(context.Background(), newMDForController("test", "default"))
			if err != nil {
				t.Fatal(err)
			}
			if objects[0].GetAnnotations()[runtimeVersionAnnotation] != version {
				t.Fatal("alpha API was mistaken for a 1.1 runtime")
			}
		})
	}
}

func TestUnknownRuntimeFailsNewDeploymentWithoutGuessing(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRESTMapper(dynamoMapper(DynamoAPIVersion, dynamoBetaVersion)).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	if _, err := r.renderResources(context.Background(), newMDForController("test", "default")); err == nil {
		t.Fatal("guessed a runtime from served APIs")
	}
}

func TestNamespaceOperatorWinsOverClusterOperator(t *testing.T) {
	objects := append(operatorRuntimeFixtures("1.1.1", "tenant-a"), operatorRuntimeFixtures("1.5.0", "")...)
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objects...).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	for namespace, want := range map[string]string{"tenant-a": "1.1.1", "tenant-b": "1.5.0"} {
		actual, err := r.discoverRuntimeVersion(context.Background(), namespace)
		if err != nil || actual != want {
			t.Fatalf("namespace %s: %s, %v", namespace, actual, err)
		}
	}
}

func TestOperatorRuntimeRejectsConflictingOrRollingMetadata(t *testing.T) {
	for _, reason := range []string{"conflicting-image", "rolling"} {
		t.Run(reason, func(t *testing.T) {
			objects := operatorRuntimeFixtures("1.5.0", "")
			deployment := objects[0].(*appsv1.Deployment)
			if reason == "conflicting-image" {
				deployment.Spec.Template.Spec.Containers[0].Image = "nvcr.io/nvidia/ai-dynamo/kubernetes-operator:1.1.1"
			} else {
				deployment.Generation = 2
			}
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objects...).Build()
			r := NewDynamoProviderReconciler(c, newScheme(), "")
			if _, err := r.discoverRuntimeVersion(context.Background(), "default"); err == nil {
				t.Fatal("ambiguous runtime accepted")
			}
		})
	}
}
