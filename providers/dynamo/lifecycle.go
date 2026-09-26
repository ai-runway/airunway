package dynamo

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// An input mismatch is not a serving failure and never authorizes teardown.
type intentLockedError struct{ message string }

func (e *intentLockedError) Error() string { return e.message }

func intentAttemptName(md *api.ModelDeployment) string {
	sum := sha256.Sum256([]byte(string(md.UID) + "\x00" + md.Annotations[dynamointent.AttemptAnnotation]))
	// Keep the request at most 32 characters, leaving space for upstream
	// profile- jobs and -dgd/component Service suffixes.
	prefix := strings.TrimRight(strings.ReplaceAll(md.Name[:min(len(md.Name), 15)], ".", "-"), "-")
	return fmt.Sprintf("%s-%x", prefix, sum[:8])
}

func ensureProviderStatus(md *api.ModelDeployment) *api.ProviderStatus {
	if md.Status.Provider == nil {
		md.Status.Provider = &api.ProviderStatus{Name: ProviderName}
	}
	return md.Status.Provider
}

func (r *DynamoProviderReconciler) findRequest(ctx context.Context, md *api.ModelDeployment) (*unstructured.Unstructured, error) {
	p := ensureProviderStatus(md)
	names := []string{md.Name, intentAttemptName(md)}
	if p.RequestRef != nil {
		if p.RequestRef.Namespace != md.Namespace || p.RequestRef.Kind != DynamoGraphDeploymentRequestKind || p.RequestRef.APIVersion != DynamoAPIGroup+"/"+DynamoGraphDeploymentRequestAPIVersion {
			return nil, fmt.Errorf("invalid Dynamo request reference")
		}
		names = []string{p.RequestRef.Name, intentAttemptName(md)}
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		u := newDynamoResource(DynamoGraphDeploymentRequestAPIVersion, DynamoGraphDeploymentRequestKind, name, md.Namespace)
		if err := r.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
			if upstreamResourceUnavailable(err) {
				continue
			}
			return nil, err
		}
		if err := verifyDynamoOwnership(u, md.UID); err != nil {
			// A colliding legacy name is not ours to migrate.
			if name == md.Name && p.RequestRef == nil {
				continue
			}
			return nil, err
		}
		if p.RequestRef != nil && p.RequestRef.Name == name && p.RequestRef.UID != "" && p.RequestRef.UID != string(u.GetUID()) {
			return nil, &resourceConflictError{namespace: md.Namespace, name: name}
		}
		return u, nil
	}
	return nil, nil
}

func (r *DynamoProviderReconciler) reconcileIntent(ctx context.Context, desired *unstructured.Unstructured, md *api.ModelDeployment) error {
	p := ensureProviderStatus(md)
	hash, err := dynamointent.Fingerprint(md)
	if err != nil {
		return err
	}
	attempt := md.Annotations[dynamointent.AttemptAnnotation]
	existing, err := r.findRequest(ctx, md)
	if err != nil {
		return err
	}
	acceptedAttempt := ""
	if p.Intent != nil {
		acceptedAttempt = p.Intent.Attempt
	}
	if existing != nil {
		acceptedAttempt = existing.GetAnnotations()[dynamointent.AttemptAnnotation]
		// Recover a successful create followed by a failed MD status write. The
		// new deterministic request already carries its accepted attempt identity.
		if p.RequestRef != nil && p.RequestRef.Name != existing.GetName() {
			if p.WorkloadRef != nil {
				old, err := r.findDGD(ctx, p.WorkloadRef.Namespace, p.WorkloadRef.Name, p.WorkloadRef)
				if err != nil {
					return err
				}
				if old != nil {
					return fmt.Errorf("new request overlaps recorded previous workload")
				}
			}
			p.WorkloadRef = nil
			p.RequestRef = resourceReference(existing)
		}
	}
	if (existing != nil || p.RequestRef != nil || p.WorkloadRef != nil) && acceptedAttempt != attempt {
		if p.Intent == nil {
			p.Intent = &api.ProviderIntentStatus{Attempt: acceptedAttempt}
		}
		// Save the exact resource identities and stop advertising old serving data
		// before the first delete. The outer reconcile persists this checkpoint.
		beforeRequest, beforeWorkload := p.RequestRef, p.WorkloadRef
		if existing != nil {
			p.RequestRef = resourceReference(existing)
			if _, err := r.resolveGeneratedDGD(ctx, md, existing); err != nil {
				return err
			}
		}
		needsCheckpoint := p.Intent.Phase != "Replacing" || !reflect.DeepEqual(beforeRequest, p.RequestRef) || !reflect.DeepEqual(beforeWorkload, p.WorkloadRef)
		p.Intent.Phase = "Replacing"
		md.Status.Phase = api.DeploymentPhaseDeploying
		md.Status.Message = "Replacing the previous Dynamo configuration attempt"
		md.Status.Endpoint = nil
		md.Status.Replicas = nil
		p.InferencePoolRef = nil
		r.setCondition(md, api.ConditionTypeReady, metav1.ConditionFalse, "Reconfiguring", md.Status.Message)
		if needsCheckpoint {
			return errIntentResourceReplacing
		}
		pending, err := r.deleteIntentResource(ctx, md, existing)
		if err != nil {
			return err
		}
		if pending {
			return errIntentResourceReplacing
		}
		p.RequestRef = nil
		p.WorkloadRef = nil
		p.Intent = nil
	}
	if existing != nil {
		desired.SetName(existing.GetName()) // adopt legacy same-name requests in place
		if existing.GetDeletionTimestamp() != nil {
			return errIntentResourceReplacing
		}
		phase, _, _ := unstructured.NestedString(existing.Object, "status", "phase")
		acceptedHash := existing.GetAnnotations()[dynamointent.HashAnnotation]
		if acceptedHash == "" && p.Intent != nil {
			acceptedHash = p.Intent.InputHash
		}
		changed := acceptedHash != "" && acceptedHash != hash
		if acceptedHash == "" {
			// Compare only supplied fields. Upstream discovery/defaulting may populate
			// other hardware, workload and metadata fields after submission.
			have, _, _ := unstructured.NestedMap(existing.Object, "spec")
			want, _, _ := unstructured.NestedMap(desired.Object, "spec")
			changed = selectedOverrideValuesDiffer(have, want, want)
		}
		if changed && (phase == "Profiling" || phase == "Ready" || phase == "Deploying" || phase == "Deployed") {
			p.RequestRef = resourceReference(existing)
			return &intentLockedError{message: "Dynamo profiling inputs are locked; change the airunway.ai/dynamo-attempt annotation to explicitly reconfigure"}
		}
		next := existing.DeepCopy()
		annotations := next.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[dynamointent.HashAnnotation] = hash
		annotations[dynamointent.AttemptAnnotation] = attempt
		next.SetAnnotations(annotations)
		if changed {
			next.Object["spec"] = desired.Object["spec"]
		}
		// Optimistic metadata patch preserves operator defaults and concurrent status.
		if !reflect.DeepEqual(existing.Object, next.Object) {
			if changed {
				err = r.Update(ctx, next, strictFieldValidation)
			} else {
				err = r.Patch(ctx, next, client.MergeFromWithOptions(existing, client.MergeFromWithOptimisticLock{}), strictFieldValidation)
			}
			if err != nil {
				return wrapResourceWriteError(err, false)
			}
		}
		p.RequestRef = resourceReference(next)
		p.Intent = &api.ProviderIntentStatus{Phase: phase, InputHash: hash, Attempt: attempt}
		return nil
	}
	// The request may have been removed out of band. Never reuse its name while
	// the same attempt's workload still exists, or silently reprofile that attempt.
	if p.RequestRef != nil && acceptedAttempt == attempt {
		return &intentLockedError{message: "The Dynamo request is missing; change airunway.ai/dynamo-attempt to explicitly retry"}
	}
	desired.SetName(intentAttemptName(md))
	annotations := desired.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[dynamointent.HashAnnotation] = hash
	annotations[dynamointent.AttemptAnnotation] = attempt
	desired.SetAnnotations(annotations)
	if err := r.Create(ctx, desired, strictFieldValidation); err != nil {
		return wrapResourceWriteError(err, false)
	}
	p.RequestRef = resourceReference(desired)
	p.Intent = &api.ProviderIntentStatus{Phase: "Pending", InputHash: hash, Attempt: attempt}
	return nil
}

// Releases 1.1.1 and 1.5 bind by name and namespace. Newer releases may also
// supply a request UID. Never ignore that stronger identity when it is present.
func generatedByRequest(dgd, dgdr *unstructured.Unstructured) bool {
	if dgdr == nil {
		return false
	}
	labels := dgd.GetLabels()
	if labels[dynamoDGDRNameLabel] != dgdr.GetName() || labels[dynamoDGDRNamespaceLabel] != dgdr.GetNamespace() {
		return false
	}
	if uid := dgd.GetAnnotations()["nvidia.com/dgdr-uid"]; uid != "" && uid != string(dgdr.GetUID()) {
		return false
	}
	created, requested := dgd.GetCreationTimestamp(), dgdr.GetCreationTimestamp()
	if !created.IsZero() && !requested.IsZero() && created.Before(&requested) {
		return false
	}
	return true
}

func (r *DynamoProviderReconciler) resolveGeneratedDGD(ctx context.Context, md *api.ModelDeployment, request *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	p := ensureProviderStatus(md)
	name, _, _ := unstructured.NestedString(request.Object, "status", "dgdName")
	if name == "" {
		if p.WorkloadRef == nil {
			return nil, nil
		}
		name = p.WorkloadRef.Name
	}
	if p.WorkloadRef != nil && p.WorkloadRef.Name != name {
		return nil, fmt.Errorf("generated Dynamo workload identity changed from %s to %s", p.WorkloadRef.Name, name)
	}
	dgd, err := r.findDGD(ctx, md.Namespace, name, p.WorkloadRef)
	if err != nil || dgd == nil {
		return dgd, err
	}
	if !generatedByRequest(dgd, request) {
		return nil, &resourceConflictError{namespace: md.Namespace, name: name}
	}
	p.WorkloadRef = resourceReference(dgd)
	return dgd, nil
}

func (r *DynamoProviderReconciler) deleteRecordedWorkload(ctx context.Context, md *api.ModelDeployment) (bool, error) {
	p := ensureProviderStatus(md)
	if p.WorkloadRef == nil {
		return false, nil
	}
	ref := p.WorkloadRef
	if ref.Namespace != md.Namespace || ref.UID == "" {
		return false, fmt.Errorf("refusing cleanup without a recorded Dynamo workload UID")
	}
	dgd, err := r.findDGD(ctx, ref.Namespace, ref.Name, ref)
	if err != nil || dgd == nil {
		return false, err
	}
	if dgd.GetDeletionTimestamp() == nil {
		if err := r.deleteWithIdentityPreconditions(ctx, dgd); err != nil && !errors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}

// Generated DGDs intentionally have no request ownerReference. Follow their
// relationship labels back to the owned request, or a persisted workload UID.
func (r *DynamoProviderReconciler) mapDynamoWorkload(ctx context.Context, obj client.Object) []reconcile.Request {
	for _, owner := range obj.GetOwnerReferences() {
		if owner.APIVersion == api.GroupVersion.String() && owner.Kind == "ModelDeployment" {
			return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: owner.Name}}}
		}
	}
	labels := obj.GetLabels()
	if name := labels[dynamoDGDRNameLabel]; name != "" && labels[dynamoDGDRNamespaceLabel] == obj.GetNamespace() {
		request := newDynamoResource(DynamoGraphDeploymentRequestAPIVersion, DynamoGraphDeploymentRequestKind, name, obj.GetNamespace())
		if err := r.Get(ctx, client.ObjectKeyFromObject(request), request); err == nil {
			for _, owner := range request.GetOwnerReferences() {
				if owner.APIVersion == api.GroupVersion.String() && owner.Kind == "ModelDeployment" {
					return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: owner.Name}}}
				}
			}
		}
	}
	var deployments api.ModelDeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	for _, md := range deployments.Items {
		if md.Status.Provider == nil || md.Status.Provider.Name != ProviderName || md.Status.Provider.WorkloadRef == nil {
			continue
		}
		ref := md.Status.Provider.WorkloadRef
		if ref.UID != "" && ref.UID == string(obj.GetUID()) && ref.Name == obj.GetName() {
			return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(&md)}}
		}
	}
	return nil
}
