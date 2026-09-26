package dynamo

import (
	"context"
	"fmt"
	"strings"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *DynamoProviderReconciler) readServingStatus(ctx context.Context, md *api.ModelDeployment, upstream *unstructured.Unstructured) (*ProviderStatusResult, error) {
	p := ensureProviderStatus(md)
	p.InferencePoolRef = nil
	result, err := r.StatusTranslator.TranslateStatus(upstream)
	if err != nil {
		return nil, err
	}
	workload := upstream
	if upstream.GetKind() == DynamoGraphDeploymentRequestKind {
		if err := verifyDynamoOwnership(upstream, md.UID); err != nil {
			return nil, err
		}
		if p.RequestRef != nil && p.RequestRef.UID != "" && p.RequestRef.UID != string(upstream.GetUID()) {
			return nil, fmt.Errorf("Dynamo request UID changed")
		}
		p.RequestRef = resourceReference(upstream)
		if p.Intent == nil {
			p.Intent = &api.ProviderIntentStatus{}
		}
		p.Intent.Phase, _, _ = unstructured.NestedString(upstream.Object, "status", "phase")
		p.Intent.ProfilingPhase, _, _ = unstructured.NestedString(upstream.Object, "status", "profilingPhase")
		if hash := upstream.GetAnnotations()[dynamointent.HashAnnotation]; hash != "" {
			p.Intent.InputHash = hash
		}
		p.Intent.Attempt = upstream.GetAnnotations()[dynamointent.AttemptAnnotation]
		workload, err = r.resolveGeneratedDGD(ctx, md, upstream)
		if err != nil {
			return nil, err
		}
		if workload == nil {
			return result, nil
		}
		result, err = r.StatusTranslator.TranslateStatus(workload)
		if err != nil {
			return nil, err
		}
	} else {
		if err := verifyDynamoOwnership(workload, md.UID); err != nil {
			return nil, err
		}
		if p.WorkloadRef != nil && p.WorkloadRef.UID != "" && p.WorkloadRef.UID != string(workload.GetUID()) {
			return nil, fmt.Errorf("Dynamo workload UID changed")
		}
		p.WorkloadRef = resourceReference(workload)
	}
	if err := r.resolveServingResources(ctx, md, workload, result); err != nil {
		return nil, err
	}
	return result, nil
}

func ownsUID(object client.Object, uid types.UID) bool {
	if uid == "" {
		return false
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.UID == uid {
			return true
		}
	}
	return false
}

func hasEPPComponent(dgd *unstructured.Unstructured) bool {
	for name, raw := range dynamoComponents(dgd.Object, "spec") {
		c, _ := raw.(map[string]any)
		kind := dynamoComponentType(c)
		if strings.EqualFold(kind, "epp") || kind == "" && strings.EqualFold(name, "epp") {
			return true
		}
	}
	return false
}

func dynamoComponentType(c map[string]any) string {
	for _, key := range []string{"type", "componentType", "dynamoComponentType"} {
		if kind, ok := c[key].(string); ok && kind != "" {
			return kind
		}
	}
	return ""
}

func (r *DynamoProviderReconciler) serviceBelongsToDGD(ctx context.Context, svc *corev1.Service, dgd *unstructured.Unstructured) (bool, error) {
	if ownsUID(svc, dgd.GetUID()) {
		return true, nil
	}
	// Without Grove, the operator creates the Service below a DCD rather than
	// directly below the DGD. Verify both links instead of trusting name labels.
	for _, owner := range svc.OwnerReferences {
		if owner.Kind != "DynamoComponentDeployment" || !strings.HasPrefix(owner.APIVersion, DynamoAPIGroup+"/") || owner.UID == "" {
			continue
		}
		component := &unstructured.Unstructured{}
		component.SetAPIVersion(owner.APIVersion)
		component.SetKind(owner.Kind)
		err := r.Get(ctx, client.ObjectKey{Namespace: svc.Namespace, Name: owner.Name}, component)
		if upstreamResourceUnavailable(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if component.GetUID() == owner.UID && ownsUID(component, dgd.GetUID()) {
			return true, nil
		}
	}
	return false, nil
}

// InferencePool names are only candidates until the actual pool, belonging to
// this DGD incarnation and selecting a real topology, has been observed.
func (r *DynamoProviderReconciler) resolveServingResources(ctx context.Context, md *api.ModelDeployment, dgd *unstructured.Unstructured, result *ProviderStatusResult) error {
	p := ensureProviderStatus(md)
	p.InferencePoolRef = nil
	if hasEPPComponent(dgd) {
		for _, version := range []string{"v1", "v1alpha2"} {
			pool := &unstructured.Unstructured{}
			pool.SetAPIVersion("inference.networking.k8s.io/" + version)
			pool.SetKind("InferencePool")
			err := r.Get(ctx, client.ObjectKey{Namespace: dgd.GetNamespace(), Name: dgd.GetName() + "-pool"}, pool)
			if upstreamResourceUnavailable(err) {
				continue
			}
			if err != nil {
				return err
			}
			if pool.GetDeletionTimestamp() != nil || !ownsUID(pool, dgd.GetUID()) {
				continue
			}
			selector, found, _ := unstructured.NestedMap(pool.Object, "spec", "selector")
			if labels, hasLabels, _ := unstructured.NestedMap(selector, "matchLabels"); !found || len(selector) == 0 || (hasLabels && len(labels) == 0) {
				continue
			}
			p.InferencePoolRef = resourceReference(pool)
			break
		}
	}
	if result.Endpoint == nil {
		return nil
	}
	// Generated deployments can choose a custom frontend component name. The
	// translator derives the candidate from that spec; verify the actual Service.
	var svc corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Namespace: dgd.GetNamespace(), Name: result.Endpoint.Service}, &svc); err != nil {
		result.Endpoint = nil
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	owned, err := r.serviceBelongsToDGD(ctx, &svc, dgd)
	if err != nil {
		return err
	}
	if svc.DeletionTimestamp != nil || !owned {
		result.Endpoint = nil
		return nil
	}
	for _, port := range svc.Spec.Ports {
		if port.Name == "http" || port.Port == result.Endpoint.Port {
			result.Endpoint.Port = port.Port
			return nil
		}
	}
	if len(svc.Spec.Ports) == 1 {
		result.Endpoint.Port = svc.Spec.Ports[0].Port
		return nil
	}
	result.Endpoint = nil
	return nil
}
