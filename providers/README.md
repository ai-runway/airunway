# Providers

> **Note:** These provider implementations are included in-tree temporarily for testing and development purposes. The intention is for all providers to live out-of-tree as independent operators.

## Reported version contract (`shimVersion` / `SHIM_VERSION`)

Every shim reports its own version through `InferenceProviderConfig.status.version`,
which `kubectl`, the Web UI, and the Headlamp plugin display. That value is
**injected at build time** — it is deliberately *not* a hand-maintained constant
(a constant is never bumped at release and silently goes stale, which is the bug
this pattern exists to prevent).

If you add a new shim, replicate the contract exactly:

1. **`config.go`** — declare the injection target as an **unexported `var` with a
   plain string literal**, then compose the public version from it:

   ```go
   // shimVersion is injected at build time via -ldflags -X; "dev" is the
   // fallback for bare `go build`/`go run`/`go test` that bypass the Makefile.
   var shimVersion = "dev"

   // ProviderVersion is written to InferenceProviderConfig.status.version.
   var ProviderVersion = ProviderConfigName + "-provider:" + shimVersion
   ```

   - Inject **`shimVersion`**, never `ProviderVersion`: `-X` can only patch a var
     whose initializer is a single string constant. `ProviderVersion` has a
     composite initializer, so `-X` on it silently no-ops. Keep `shimVersion`
     unexported — `-X` resolves a linker symbol regardless of Go visibility.
   - Both must be `var`, not `const` (`-X` cannot touch a `const`, and a `const`
     cannot reference a `var`).

2. **`Makefile`** — resolve the module path with `go list -m` (never hand-type it)
   and feed both a release tag and a git-stamp default through one `-X`:

   ```makefile
   MODULE       := $(shell go list -m)
   GIT_SHA      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
   GIT_DIRTY    := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo '-dirty')
   SHIM_VERSION ?= dev-$(GIT_SHA)$(GIT_DIRTY)
   LDFLAGS      += -X $(MODULE).shimVersion=$(SHIM_VERSION)
   ```

   Pass `--build-arg SHIM_VERSION=$(SHIM_VERSION)` to `docker-build`.

3. **`Dockerfile`** — declare `ARG SHIM_VERSION` **with no default** and fail loud
   if it is missing, so a bare `docker build` cannot ship `:dev` under a real
   release tag. Resolve the module path the same way:

   ```dockerfile
   ARG SHIM_VERSION
   RUN test -n "${SHIM_VERSION}" || (echo "ERROR: SHIM_VERSION build arg is required; pass --build-arg SHIM_VERSION=..." >&2; exit 1)
   RUN cd providers/<name> && MODULE=$(go list -m) && \
       go build -ldflags="-X ${MODULE}.shimVersion=${SHIM_VERSION}" -o provider cmd/main.go
   ```

4. **Release workflow** — pass `SHIM_VERSION=${{ inputs.version }}` (the same value
   that tags the image) in the `build-args:` block, so `status.version` equals the
   image tag by construction.

5. **Tests** — assert the *shape* (`strings.HasPrefix(ProviderVersion, "<name>-provider:")`),
   not an exact literal, and include a `TestShimVersionInjection` that asserts the
   **runtime** value under injection (gated on `EXPECT_PROVIDER_VERSION` so plain
   `go test` skips). The CI matrix in `.github/workflows/test.yml` runs it built
   with `-ldflags` so a silent `-X` no-op fails the build.
