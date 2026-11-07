package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

var _ = Describe("ModelDeployment status conflicts", func() {
	DescribeTable("preserves concurrent provider status and retries from a fresh read",
		func(rejectedSwitch bool, phase airunwayv1alpha1.DeploymentPhase, message string) {
			md := newProviderSwitchMD("status-conflict", "dynamo", "dynamo")
			initialStatus := md.Status.DeepCopy()
			md.Status = airunwayv1alpha1.ModelDeploymentStatus{}
			Expect(k8sClient.Create(ctx, md)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, md)).To(Succeed()) })
			md.Status = *initialStatus
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			condition := metav1.Condition{
				Type: airunwayv1alpha1.ConditionTypeValidated, Status: metav1.ConditionFalse,
				Reason: "ValidationFailed", Message: "previous validation failure", LastTransitionTime: metav1.Now(),
			}
			md.Status.Message = "Validation failed: " + condition.Message
			if rejectedSwitch {
				condition.Type = airunwayv1alpha1.ConditionTypeProviderSelected
				condition.Reason = providerChangeNotSupportedReason
				condition.Message = "changing spec.provider.name from dynamo to vllm is not supported"
				md.Status.Message = condition.Message
			}
			md.Status.Conditions = []metav1.Condition{condition}
			Expect(k8sClient.Status().Update(ctx, md)).To(Succeed())

			// Both writes go to the real API server. Interception only fixes their
			// ordering so provider progress lands after the core's read.
			apiClient, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			patchCalls := 0
			var providerStatus airunwayv1alpha1.ModelDeploymentStatus
			var providerResourceVersion string
			intercepted := interceptor.NewClient(apiClient, interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					Expect(subresource).To(Equal("status"))
					patchCalls++
					if patchCalls == 1 {
						fresh := &airunwayv1alpha1.ModelDeployment{}
						Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(md), fresh)).To(Succeed())
						Expect(fresh.ResourceVersion).To(Equal(obj.GetResourceVersion()))
						fresh.Status.Phase = phase
						fresh.Status.Message = message
						fresh.Status.Provider.ResourceName = "provider-workload"
						fresh.Status.Provider.ResourceKind = "DynamoGraphDeployment"
						ready := metav1.ConditionFalse
						if phase == airunwayv1alpha1.DeploymentPhaseRunning {
							ready = metav1.ConditionTrue
						}
						meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
							Type: airunwayv1alpha1.ConditionTypeReady, Status: ready,
							Reason: "ProviderProgress", Message: message, ObservedGeneration: fresh.Generation,
						})
						Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
						Expect(fresh.ResourceVersion).NotTo(Equal(obj.GetResourceVersion()))
						providerResourceVersion = fresh.ResourceVersion
						providerStatus = *fresh.Status.DeepCopy()
					}
					return c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
				},
			})
			r := &ModelDeploymentReconciler{Client: intercepted, Scheme: k8sClient.Scheme()}
			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(md)}

			_, err = r.Reconcile(ctx, req)
			Expect(apierrors.IsConflict(err)).To(BeTrue(), "stale status patch must return an API conflict: %v", err)
			fresh := &airunwayv1alpha1.ModelDeployment{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, fresh)).To(Succeed())
			Expect(fresh.ResourceVersion).To(Equal(providerResourceVersion))
			Expect(fresh.Status).To(Equal(providerStatus))

			// controller-runtime retries returned conflicts; the next reconcile
			// must read the provider's update and recompute its status patch.
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(patchCalls).To(Equal(2))
			Expect(k8sClient.Get(ctx, req.NamespacedName, fresh)).To(Succeed())
			Expect(fresh.Status.Phase).To(Equal(providerStatus.Phase))
			Expect(fresh.Status.Message).To(Equal(providerStatus.Message))
			Expect(fresh.Status.Provider).To(Equal(providerStatus.Provider))
			Expect(meta.FindStatusCondition(fresh.Status.Conditions, airunwayv1alpha1.ConditionTypeReady)).To(
				Equal(meta.FindStatusCondition(providerStatus.Conditions, airunwayv1alpha1.ConditionTypeReady)))
			Expect(meta.IsStatusConditionTrue(fresh.Status.Conditions, condition.Type)).To(BeTrue())
		},
		Entry("validation recovery with provider progress", false, airunwayv1alpha1.DeploymentPhaseDeploying, "Waiting for provider pods"),
		Entry("validation recovery with provider readiness", false, airunwayv1alpha1.DeploymentPhaseRunning, "Provider is ready"),
		Entry("switch recovery with provider progress", true, airunwayv1alpha1.DeploymentPhaseDeploying, "Waiting for provider pods"),
		Entry("switch recovery with provider readiness", true, airunwayv1alpha1.DeploymentPhaseRunning, "Provider is ready"),
		Entry("switch recovery with provider failure", true, airunwayv1alpha1.DeploymentPhaseFailed, "Provider workload failed"),
	)
})
