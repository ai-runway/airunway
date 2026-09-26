package dynamo

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

type runtimeDiscoveryError struct{ err error }

func (e *runtimeDiscoveryError) Error() string { return e.err.Error() }
func (e *runtimeDiscoveryError) Unwrap() error { return e.err }

// API discovery says which representations can be read. It does not identify
// the controller image or runtime contract, especially with namespace operators.
func (r *DynamoProviderReconciler) discoverRuntimeVersion(ctx context.Context, namespace string) (string, error) {
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.MatchingLabels{"app.kubernetes.io/part-of": "dynamo-operator"}); err != nil {
		return "", fmt.Errorf("cannot discover Dynamo operator version: %w", err)
	}
	scoped, global := []*appsv1.Deployment{}, []*appsv1.Deployment{}
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		if deployment.DeletionTimestamp != nil {
			continue
		}
		target, err := r.operatorNamespace(ctx, deployment)
		if err != nil {
			return "", err
		}
		if target == namespace {
			scoped = append(scoped, deployment)
		} else if target == "" {
			global = append(global, deployment)
		}
	}
	candidates := scoped
	if len(candidates) == 0 {
		candidates = global
	}
	version := ""
	for _, deployment := range candidates {
		if deployment.Status.AvailableReplicas < 1 || deployment.Status.ObservedGeneration < deployment.Generation || deployment.Status.UpdatedReplicas < deployment.Status.Replicas {
			return "", fmt.Errorf("Dynamo operator %s/%s is not fully rolled out; wait for its upgrade before creating a new deployment", deployment.Namespace, deployment.Name)
		}
		candidate, err := operatorDeploymentVersion(deployment)
		if err != nil {
			return "", err
		}
		if version != "" && version != candidate {
			return "", fmt.Errorf("multiple Dynamo operators have conflicting runtime versions for namespace %s", namespace)
		}
		version = candidate
	}
	if version == "" {
		return "", fmt.Errorf("cannot determine the Dynamo runtime for namespace %s: no matching operator Deployment metadata; install or label the operator and allow Deployment/ConfigMap reads", namespace)
	}
	return version, nil
}

func (r *DynamoProviderReconciler) operatorNamespace(ctx context.Context, deployment *appsv1.Deployment) (string, error) {
	// The released Helm charts mount operator-config/config.yaml in the manager.
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name != "operator-config" || volume.ConfigMap == nil {
			continue
		}
		var config corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKey{Namespace: deployment.Namespace, Name: volume.ConfigMap.Name}, &config); err != nil {
			return "", fmt.Errorf("cannot read Dynamo operator namespace configuration: %w", err)
		}
		raw, found := config.Data["config.yaml"]
		if !found || strings.TrimSpace(raw) == "" {
			return "", fmt.Errorf("Dynamo operator configuration is missing config.yaml")
		}
		var contents map[string]any
		if err := yaml.Unmarshal([]byte(raw), &contents); err != nil {
			return "", fmt.Errorf("invalid Dynamo operator namespace configuration: %w", err)
		}
		target, _, err := unstructured.NestedString(contents, "namespace", "restricted")
		return target, err
	}
	// A custom installation without this configuration is not evidence of scope.
	return "", fmt.Errorf("cannot determine namespace scope for Dynamo operator %s/%s: expected operator-config ConfigMap", deployment.Namespace, deployment.Name)
}

func operatorDeploymentVersion(deployment *appsv1.Deployment) (string, error) {
	candidates := []string{deployment.Labels["app.kubernetes.io/version"]}
	foundManager := false
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != "manager" {
			continue
		}
		foundManager = true
		if tag := semanticImageTag(container.Image); tag != "" {
			candidates = append(candidates, tag)
		}
		for i, arg := range container.Args {
			if strings.HasPrefix(arg, "--operator-version=") {
				candidates = append(candidates, strings.TrimPrefix(arg, "--operator-version="))
			}
			if arg == "--operator-version" && i+1 < len(container.Args) {
				candidates = append(candidates, container.Args[i+1])
			}
		}
	}
	version := ""
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		parsed, err := utilversion.ParseSemantic(candidate)
		if err != nil {
			return "", fmt.Errorf("Dynamo operator %s/%s has unrecognized version metadata %q", deployment.Namespace, deployment.Name, candidate)
		}
		normalized := parsed.String()
		if version != "" && normalized != version {
			return "", fmt.Errorf("Dynamo operator %s/%s image and version metadata disagree", deployment.Namespace, deployment.Name)
		}
		version = normalized
	}
	if !foundManager || version == "" {
		return "", fmt.Errorf("Dynamo operator %s/%s lacks an identifiable manager runtime version", deployment.Namespace, deployment.Name)
	}
	return version, nil
}

func semanticImageTag(image string) string {
	// A tag plus digest still provides an explicit version. A digest alone does not.
	image = strings.SplitN(image, "@", 2)[0]
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i+1:], "/") {
		if parsed, err := utilversion.ParseSemantic(image[i+1:]); err == nil {
			return parsed.String()
		}
	}
	return ""
}
