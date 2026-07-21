package vllm

import (
	"context"
	"errors"
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeImageResolver struct {
	results map[string]*ResolvedImage
	errors  map[string]error
	calls   []string
}

func (f *fakeImageResolver) Resolve(ctx context.Context, imageRef string) (*ResolvedImage, error) {
	f.calls = append(f.calls, imageRef)
	if err := f.errors[imageRef]; err != nil {
		return nil, err
	}
	resolved, ok := f.results[imageRef]
	if !ok {
		return nil, errors.New("unexpected image resolution request")
	}
	copy := *resolved
	return &copy, nil
}

func successfulFakeResolver(images ...*ResolvedImage) *fakeImageResolver {
	resolver := &fakeImageResolver{
		results: map[string]*ResolvedImage{},
		errors:  map[string]error{},
	}
	for _, image := range images {
		resolver.results[image.Requested] = image
	}
	return resolver
}

func failingFakeResolver(imageRef string, err error) *fakeImageResolver {
	return &fakeImageResolver{
		results: map[string]*ResolvedImage{},
		errors:  map[string]error{imageRef: err},
	}
}

func fakeResolvedImage(imageRef, digest string) *ResolvedImage {
	repository, tag, _ := parseImageReference(imageRef)
	return &ResolvedImage{
		Requested:  imageRef,
		Resolved:   repository + "@" + digest,
		Repository: repository,
		Tag:        tag,
		Digest:     digest,
		CreatedAt:  "2026-05-01T00:00:00Z",
		Revision:   "abc123",
	}
}

func TestSetImageResolutionStatusDefaultImage(t *testing.T) {
	md := newMDForController("default-image", "default")
	resolver := successfulFakeResolver(fakeResolvedImage(DefaultVLLMImage, "sha256:default"))
	r := &VLLMProviderReconciler{ImageResolver: resolver}

	if err := r.setImageResolutionStatus(context.Background(), md); err != nil {
		t.Fatalf("setImageResolutionStatus() error = %v", err)
	}

	if len(resolver.calls) != 1 || resolver.calls[0] != DefaultVLLMImage {
		t.Fatalf("resolver calls = %#v, want [%q]", resolver.calls, DefaultVLLMImage)
	}
	image := requireImageStatus(t, md)
	if image.Requested != DefaultVLLMImage {
		t.Fatalf("requested image = %q, want %q", image.Requested, DefaultVLLMImage)
	}
	if image.Resolved != "vllm/vllm-openai@sha256:default" {
		t.Fatalf("resolved image = %q, want %q", image.Resolved, "vllm/vllm-openai@sha256:default")
	}
	if image.Repository != "vllm/vllm-openai" {
		t.Fatalf("repository = %q, want %q", image.Repository, "vllm/vllm-openai")
	}
	if image.Tag != "cu130-nightly" {
		t.Fatalf("tag = %q, want %q", image.Tag, "cu130-nightly")
	}
	if image.Digest != "sha256:default" {
		t.Fatalf("digest = %q, want %q", image.Digest, "sha256:default")
	}
	if image.CreatedAt != "2026-05-01T00:00:00Z" {
		t.Fatalf("createdAt = %q, want %q", image.CreatedAt, "2026-05-01T00:00:00Z")
	}
	if image.Revision != "abc123" {
		t.Fatalf("revision = %q, want %q", image.Revision, "abc123")
	}
	if image.Source != "nightly" {
		t.Fatalf("source = %q, want %q", image.Source, "nightly")
	}
	if !image.InNightly {
		t.Fatal("inNightly = false, want true for the Direct vLLM default nightly image")
	}
	if image.Verified {
		t.Fatal("verified = true, want false because verification is not implemented")
	}
	if image.VerificationMessage != imageVerificationNotImplementedMessage {
		t.Fatalf("verificationMessage = %q, want %q", image.VerificationMessage, imageVerificationNotImplementedMessage)
	}

	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionTrue, "ImageResolved")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageVerified, metav1.ConditionUnknown, "VerificationNotImplemented")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeUnsupportedImage, metav1.ConditionFalse, "SupportedImage")
	if cond := meta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeRecipeResolved); cond != nil {
		t.Fatalf("RecipeResolved condition should not be set without recipe provenance annotations: %#v", cond)
	}
}

func TestSetImageResolutionStatusReusesCachedDigestForSameRequestedImage(t *testing.T) {
	md := newMDForController("cached-image", "default")
	md.Status.Image = &airunwayv1alpha1.ImageStatus{
		Requested: DefaultVLLMImage,
		Resolved:  "vllm/vllm-openai@sha256:cached",
		Digest:    "sha256:cached",
		CreatedAt: "2026-05-01T00:00:00Z",
		Revision:  "abc123",
	}
	resolver := successfulFakeResolver(fakeResolvedImage(DefaultVLLMImage, "sha256:fresh"))
	r := &VLLMProviderReconciler{ImageResolver: resolver}

	if err := r.setImageResolutionStatus(context.Background(), md); err != nil {
		t.Fatalf("setImageResolutionStatus() error = %v", err)
	}
	if len(resolver.calls) != 0 {
		t.Fatalf("resolver calls = %#v, want none when status.image already resolved current request", resolver.calls)
	}

	image := requireImageStatus(t, md)
	if image.Resolved != "vllm/vllm-openai@sha256:cached" {
		t.Fatalf("resolved image = %q, want cached digest", image.Resolved)
	}
	if image.Digest != "sha256:cached" {
		t.Fatalf("digest = %q, want cached digest", image.Digest)
	}
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionTrue, "ImageResolutionReused")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageVerified, metav1.ConditionUnknown, "VerificationNotImplemented")
}

func TestSetImageResolutionStatusStableImage(t *testing.T) {
	const stableImage = "vllm/vllm-openai:latest"
	md := newMDForController("stable-image", "default")
	md.Spec.Engine.Image = stableImage
	resolver := successfulFakeResolver(fakeResolvedImage(stableImage, "sha256:stable"))
	r := &VLLMProviderReconciler{ImageResolver: resolver}

	if err := r.setImageResolutionStatus(context.Background(), md); err != nil {
		t.Fatalf("setImageResolutionStatus() error = %v", err)
	}

	image := requireImageStatus(t, md)
	if image.Requested != stableImage {
		t.Fatalf("requested image = %q, want %q", image.Requested, stableImage)
	}
	if image.Resolved != "vllm/vllm-openai@sha256:stable" {
		t.Fatalf("resolved image = %q, want %q", image.Resolved, "vllm/vllm-openai@sha256:stable")
	}
	if image.Repository != "vllm/vllm-openai" {
		t.Fatalf("repository = %q, want %q", image.Repository, "vllm/vllm-openai")
	}
	if image.Tag != "latest" {
		t.Fatalf("tag = %q, want %q", image.Tag, "latest")
	}
	if image.Source != "stable" {
		t.Fatalf("source = %q, want %q", image.Source, "stable")
	}
	if image.InNightly {
		t.Fatal("inNightly = true, want false for the stable image")
	}
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionTrue, "ImageResolved")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageVerified, metav1.ConditionUnknown, "VerificationNotImplemented")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeUnsupportedImage, metav1.ConditionFalse, "SupportedImage")
}

func TestSetImageResolutionStatusCustomDigestImageDoesNotCallResolver(t *testing.T) {
	const customImage = "ghcr.io/acme/vllm-openai-custom:cuda12@sha256:abc123"
	md := newMDForController("custom-image", "default")
	md.Spec.Engine.Image = customImage
	resolver := successfulFakeResolver()
	r := &VLLMProviderReconciler{ImageResolver: resolver}

	if err := r.setImageResolutionStatus(context.Background(), md); err != nil {
		t.Fatalf("setImageResolutionStatus() error = %v", err)
	}

	if len(resolver.calls) != 0 {
		t.Fatalf("resolver calls = %#v, want none for digest-pinned image", resolver.calls)
	}
	image := requireImageStatus(t, md)
	if image.Requested != customImage {
		t.Fatalf("requested image = %q, want %q", image.Requested, customImage)
	}
	if image.Resolved != customImage {
		t.Fatalf("resolved image = %q, want %q", image.Resolved, customImage)
	}
	if image.Repository != "ghcr.io/acme/vllm-openai-custom" {
		t.Fatalf("repository = %q, want %q", image.Repository, "ghcr.io/acme/vllm-openai-custom")
	}
	if image.Tag != "cuda12" {
		t.Fatalf("tag = %q, want %q", image.Tag, "cuda12")
	}
	if image.Digest != "sha256:abc123" {
		t.Fatalf("digest = %q, want %q", image.Digest, "sha256:abc123")
	}
	if image.Source != "custom" {
		t.Fatalf("source = %q, want %q", image.Source, "custom")
	}
	if image.InNightly {
		t.Fatal("inNightly = true, want false for custom images")
	}
	if image.Verified {
		t.Fatal("verified = true, want false because verification is not implemented")
	}
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionTrue, "ImageResolved")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeUnsupportedImage, metav1.ConditionTrue, "CustomImage")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageVerified, metav1.ConditionUnknown, "VerificationNotImplemented")
}

func TestSetImageResolutionStatusDefaultResolutionFailureReturnsError(t *testing.T) {
	md := newMDForController("default-failure", "default")
	r := &VLLMProviderReconciler{ImageResolver: failingFakeResolver(DefaultVLLMImage, errors.New("registry unavailable"))}

	err := r.setImageResolutionStatus(context.Background(), md)
	if err == nil {
		t.Fatal("setImageResolutionStatus() error = nil, want default image resolution failure")
	}
	if !strings.Contains(err.Error(), "failed to resolve default vLLM image") {
		t.Fatalf("error = %q, want default image resolution context", err.Error())
	}

	image := requireImageStatus(t, md)
	if image.Requested != DefaultVLLMImage {
		t.Fatalf("requested image = %q, want %q", image.Requested, DefaultVLLMImage)
	}
	if image.Resolved != "" {
		t.Fatalf("resolved image = %q, want empty on resolution failure", image.Resolved)
	}
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionFalse, "ImageResolutionFailed")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageVerified, metav1.ConditionUnknown, "VerificationNotImplemented")
}

func TestSetImageResolutionStatusUserImageResolutionFailureDoesNotReturnError(t *testing.T) {
	const customImage = "ghcr.io/acme/vllm-openai-custom:cuda12"
	md := newMDForController("custom-failure", "default")
	md.Spec.Engine.Image = customImage
	resolver := failingFakeResolver(customImage, errors.New("registry unavailable"))
	r := &VLLMProviderReconciler{ImageResolver: resolver}

	if err := r.setImageResolutionStatus(context.Background(), md); err != nil {
		t.Fatalf("setImageResolutionStatus() error = %v, want nil for user-specified image resolution failure", err)
	}
	if len(resolver.calls) != 1 || resolver.calls[0] != customImage {
		t.Fatalf("resolver calls = %#v, want [%q]", resolver.calls, customImage)
	}

	image := requireImageStatus(t, md)
	if image.Requested != customImage {
		t.Fatalf("requested image = %q, want %q", image.Requested, customImage)
	}
	if image.Resolved != "" {
		t.Fatalf("resolved image = %q, want empty on resolution failure", image.Resolved)
	}
	if image.Source != "custom" {
		t.Fatalf("source = %q, want custom", image.Source)
	}
	if !strings.Contains(image.Message, "Continuing with the user-specified image reference") {
		t.Fatalf("image message = %q, want user-image continuation", image.Message)
	}
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionFalse, "ImageResolutionFailed")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeUnsupportedImage, metav1.ConditionTrue, "CustomImage")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageVerified, metav1.ConditionUnknown, "VerificationNotImplemented")
}

func TestSetImageResolutionStatusRecipeProvenanceIsNotVerification(t *testing.T) {
	const stableImage = "vllm/vllm-openai:latest"
	md := newMDForController("recipe-image", "default")
	md.Spec.Engine.Image = stableImage
	md.Annotations = map[string]string{
		recipeGeneratedByAnnotation:          recipeGeneratedByValue,
		recipeAnnotationPrefix + "source":    "vllm-recipes",
		recipeAnnotationPrefix + "id":        "llama-3.1-8b",
		recipeAnnotationPrefix + "strategy":  "official-vllm-image",
		recipeAnnotationPrefix + "hardware":  "h100",
		recipeAnnotationPrefix + "precision": "fp16",
		recipeAnnotationPrefix + "revision":  "abc123",
		recipeAnnotationPrefix + "features":  `["openai-api"]`,
		"airunway.ai/unrelated-annotation":   "ignored",
	}
	resolver := successfulFakeResolver(fakeResolvedImage(stableImage, "sha256:recipe"))
	r := &VLLMProviderReconciler{ImageResolver: resolver}

	if err := r.setImageResolutionStatus(context.Background(), md); err != nil {
		t.Fatalf("setImageResolutionStatus() error = %v", err)
	}

	image := requireImageStatus(t, md)
	for _, want := range []string{
		"Recipe provenance:",
		"generated-by=vllm-recipe-resolver",
		"source=vllm-recipes",
		"id=llama-3.1-8b",
		"strategy=official-vllm-image",
		"hardware=h100",
		"precision=fp16",
		"revision=abc123",
		`features=["openai-api"]`,
	} {
		if !strings.Contains(image.Message, want) {
			t.Fatalf("image message %q does not contain %q", image.Message, want)
		}
	}
	if image.Verified {
		t.Fatal("verified = true, want false because recipe provenance is not image verification")
	}

	recipeCondition := assertCondition(t, md, airunwayv1alpha1.ConditionTypeRecipeResolved, metav1.ConditionTrue, "RecipeProvenanceResolved")
	if !strings.Contains(recipeCondition.Message, "source=vllm-recipes") || !strings.Contains(recipeCondition.Message, "id=llama-3.1-8b") {
		t.Fatalf("RecipeResolved message = %q, want recipe provenance details", recipeCondition.Message)
	}
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionTrue, "ImageResolved")
	assertCondition(t, md, airunwayv1alpha1.ConditionTypeImageVerified, metav1.ConditionUnknown, "VerificationNotImplemented")
}

func requireImageStatus(t *testing.T, md *airunwayv1alpha1.ModelDeployment) *airunwayv1alpha1.ImageStatus {
	t.Helper()
	if md.Status.Image == nil {
		t.Fatal("status.image was not set")
	}
	return md.Status.Image
}

func assertCondition(t *testing.T, md *airunwayv1alpha1.ModelDeployment, conditionType string, status metav1.ConditionStatus, reason string) *metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(md.Status.Conditions, conditionType)
	if condition == nil {
		t.Fatalf("condition %q was not set", conditionType)
	}
	if condition.Status != status {
		t.Fatalf("condition %q status = %q, want %q", conditionType, condition.Status, status)
	}
	if condition.Reason != reason {
		t.Fatalf("condition %q reason = %q, want %q", conditionType, condition.Reason, reason)
	}
	return condition
}
