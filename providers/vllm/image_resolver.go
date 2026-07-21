package vllm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ImageResolver resolves container image references to immutable digests and
// best-effort image metadata.
type ImageResolver interface {
	Resolve(ctx context.Context, imageRef string) (*ResolvedImage, error)
}

// imageResolveTimeout bounds a single registry round-trip. The reconcile context
// from controller-runtime has no deadline, so without this a hung or slow
// registry would block the reconcile worker indefinitely.
const imageResolveTimeout = 30 * time.Second

// ResolvedImage contains digest resolution and provenance metadata for an image.
type ResolvedImage struct {
	Requested  string
	Resolved   string
	Repository string
	Tag        string
	Digest     string
	CreatedAt  string
	Revision   string
}

// RemoteImageResolver resolves images by querying remote registries with
// go-containerregistry.
type RemoteImageResolver struct{}

// NewRemoteImageResolver creates a remote registry-backed ImageResolver.
func NewRemoteImageResolver() *RemoteImageResolver {
	return &RemoteImageResolver{}
}

// Resolve resolves imageRef to its registry digest. It also attempts to read the
// image config for common OCI provenance labels, but treats that metadata as
// best-effort because some registries/images may not expose it.
func (r *RemoteImageResolver) Resolve(ctx context.Context, imageRef string) (*ResolvedImage, error) {
	requested := strings.TrimSpace(imageRef)
	if requested == "" {
		return nil, fmt.Errorf("image reference is empty")
	}

	ref, err := name.ParseReference(requested, name.WeakValidation)
	if err != nil {
		return nil, fmt.Errorf("parse image reference %q: %w", requested, err)
	}

	// Bound the registry round-trip so a hung registry cannot stall the worker.
	resolveCtx, cancel := context.WithTimeout(ctx, imageResolveTimeout)
	defer cancel()

	desc, err := remote.Get(ref, remote.WithContext(resolveCtx))
	if err != nil {
		return nil, fmt.Errorf("resolve image reference %q: %w", requested, err)
	}

	repository, tag, _ := parseImageReference(requested)
	digest := desc.Digest.String()
	resolved := repository + "@" + digest

	result := &ResolvedImage{
		Requested:  requested,
		Resolved:   resolved,
		Repository: repository,
		Tag:        tag,
		Digest:     digest,
	}

	if img, err := desc.Image(); err == nil {
		if cfg, err := img.ConfigFile(); err == nil && cfg != nil {
			if !cfg.Created.Time.IsZero() {
				result.CreatedAt = cfg.Created.Time.UTC().Format(time.RFC3339)
			}
			if labels := cfg.Config.Labels; len(labels) > 0 {
				if result.CreatedAt == "" {
					result.CreatedAt = strings.TrimSpace(labels["org.opencontainers.image.created"])
				}
				result.Revision = strings.TrimSpace(labels["org.opencontainers.image.revision"])
			}
		}
	}

	return result, nil
}
