# Providers

> **Note:** These provider implementations are included in-tree temporarily for testing and development purposes. The intention is for all providers to live out-of-tree as independent operators.

## Inference providers

- `providers/dynamo`
- `providers/kaito`
- `providers/kuberay`
- `providers/llmd`
- `providers/vllm`

## Agent providers

Agent provider shims are split into separate modules so they can run out-of-tree as independent controllers:

- `providers/agent-container`
- `providers/agent-kagent`
- `providers/agent-orka`

These binaries currently reuse reconciler implementations exported through `controller/pkg/agentproviders`, which is the extraction seam used to decouple provider deployment from the main controller binary. The singular `controller/pkg/agentprovider` package contains the shared provider contract.

## Reported version contract (`shimVersion` / `SHIM_VERSION`)

Every shim reports its own version through the corresponding provider resource: inference shims use `InferenceProviderConfig.status.version`, while agent shims use `AgentProviderConfig.status.version`. `kubectl`, the Web UI, and the Headlamp plugin display that value. It is **injected at build time** — deliberately *not* a hand-maintained constant (a constant is never bumped at release and silently goes stale, which is the bug this pattern exists to prevent).

If you add a new shim, replicate the contract exactly:

1. **`config.go`** — declare the injection target as an **unexported `var` with a plain string literal**, then compose the public version from it:

   ```go
   // shimVersion is injected at build time via -ldflags -X; "dev" is the
   // fallback for bare `go build`/`go run`/`go test` that bypass the Makefile.
   var shimVersion = "dev"

   // ProviderVersion is written to the provider resource's status.version.
   var ProviderVersion = ProviderConfigName + "-provider:" + shimVersion
   ```

   - Inject **`shimVersion`**, never `ProviderVersion`: `-X` can only patch a var whose initializer is a single string constant. `ProviderVersion` has a composite initializer, so `-X` on it silently no-ops. Keep `shimVersion` unexported — `-X` resolves a linker symbol regardless of Go visibility.
   - Both must be `var`, not `const` (`-X` cannot touch a `const`, and a `const` cannot reference a `var`).

2. **`Makefile`** — resolve the module path from the shim command package (never hand-type it) and feed both a release tag and a git-stamp default through one `-X`:

   ```makefile
   MODULE       := $(shell go list -f '{{.Module.Path}}' ./cmd)
   GIT_SHA      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
   GIT_DIRTY    := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo '-dirty')
   SHIM_VERSION ?= dev-$(GIT_SHA)$(GIT_DIRTY)
   LDFLAGS      += -X $(MODULE).shimVersion=$(SHIM_VERSION)
   ```

   Pass `--build-arg SHIM_VERSION=$(SHIM_VERSION)` to `docker-build`.

3. **`Dockerfile`** — declare `ARG SHIM_VERSION` **with no default** and fail loud if it is missing, so a bare `docker build` cannot ship `:dev` under a real release tag. Resolve the module path from the provider module rather than hand-typing it:

   ```dockerfile
   ARG SHIM_VERSION
   RUN test -n "${SHIM_VERSION}" || (echo "ERROR: SHIM_VERSION build arg is required; pass --build-arg SHIM_VERSION=..." >&2; exit 1)
   RUN cd providers/<name> && MODULE=$(go list -m) && \
       go build -ldflags="-X ${MODULE}.shimVersion=${SHIM_VERSION}" -o provider cmd/main.go
   ```

4. **Release workflow** — pass the exact published image version to `SHIM_VERSION`. With `docker/metadata-action`, make the selected raw tag the highest-priority tag and pass `SHIM_VERSION=${{ steps.meta.outputs.version }}` so the reported version still matches after Docker tag sanitization. In workflows that spell out tags directly, reuse the same validated `IMAGE_VERSION` for the tag and build argument.

5. **Tests** — assert the *shape* (`strings.HasPrefix(ProviderVersion, "<name>-provider:")`), not an exact literal, and include a `TestShimVersionInjection` that asserts the **runtime** value under injection (gated on `EXPECT_PROVIDER_VERSION` so plain `go test` skips). The CI matrix in `.github/workflows/test.yml` runs it built with `-ldflags` so a silent `-X` no-op fails the build.
