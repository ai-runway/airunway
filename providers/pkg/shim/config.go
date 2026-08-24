package shim

import (
	"context"
	"fmt"
	"maps"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func RegisterProviderConfig(
	ctx context.Context,
	kubeClient client.Client,
	name string,
	annotations map[string]string,
	spec airunwayv1alpha1.InferenceProviderConfigSpec,
) error {
	logger := log.FromContext(ctx)
	config := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
		Spec: spec,
	}

	existing := &airunwayv1alpha1.InferenceProviderConfig{}
	err := kubeClient.Get(ctx, types.NamespacedName{Name: name}, existing)

	if apierrors.IsNotFound(err) {
		logger.Info("Creating InferenceProviderConfig", "name", name)
		if err := kubeClient.Create(ctx, config); err != nil {
			return fmt.Errorf("failed to create InferenceProviderConfig: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get InferenceProviderConfig: %w", err)
	} else {
		existing.Spec = config.Spec
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string)
		}
		maps.Copy(existing.Annotations, annotations)
		logger.Info("Updating InferenceProviderConfig", "name", name)
		if err := kubeClient.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update InferenceProviderConfig: %w", err)
		}
	}

	return nil
}
