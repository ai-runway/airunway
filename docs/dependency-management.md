# Dependency Management

This page describes how AI Runway adds, pins, updates and monitors third-party dependencies across the controller, providers, web UI, Headlamp plugin, docs website, container images and CI workflows.

## Where dependencies are declared

| Ecosystem | Declared in | Pinned by |
| --- | --- | --- |
| Go modules | `go.mod` in `controller/`, each provider under `providers/` and `providers/pkg/shim/` | `go.sum` |
| npm packages | Root `package.json` and its workspaces (`frontend`, `backend`, `shared`, `plugins/headlamp`), plus `website/package.json` | `bun.lock` and `website/bun.lock` |
| Python packages | `requirements.txt` in `images/agents/crewai/` and `images/agents/langgraph/`; `images/model-downloader/Dockerfile` | `requirements.lock` with hashes, installed with `pip install --require-hashes`. The model downloader pins `huggingface_hub` in its `Dockerfile`, but not `click` or transitive dependencies |
| GitHub Actions | `.github/workflows/` | Full commit SHA, with the release tag in a trailing comment |
| Upstream components (Gateway API Inference Extension, provider versions, Istio, agent base images) | `versions.env` | `make verify-versions` fails when a copy in the code drifts from `versions.env` |
| Container base images | `Dockerfile`s and `versions.env` | Image tags; the agent runtime images and agent providers pin image digests |

### Known gaps

These aren't pinned yet:

- CI and the release workflow install Bun with `bun-version: latest`.
- The end-to-end workflows install kind with `go install sigs.k8s.io/kind@latest`.
- The model downloader image installs `click` without a version.

## Adding a dependency

- Prefer the standard library or a dependency the repository already uses.
- Explain in the pull request why the new dependency is needed. Check that it is actively maintained and that its license is allowed by the [CNCF third-party license policy](https://github.com/cncf/foundation/blob/main/policies-guidance/allowed-third-party-license-policy.md).
- Commit the lockfile with the change. CI installs npm packages with `bun ci`, which fails if `bun.lock` is out of date. For Go, run `go mod tidy` in the module you changed. For the Python agent images, regenerate `requirements.lock` with the command at the top of that file.
- Pin new GitHub Actions to a full commit SHA and note the release tag in a comment, as the existing workflows do.
- Bump upstream component versions in `versions.env` and follow the steps at the top of that file.

Dependency changes go through the same pull request review and CI as any other change.

## Keeping dependencies up to date

- Dependabot opens weekly pull requests for GitHub Actions and for the npm packages in the root workspace. See [`.github/dependabot.yml`](https://github.com/ai-runway/airunway/blob/main/.github/dependabot.yml).
- Dependabot security updates are enabled, so ecosystems without scheduled version updates, such as the Go modules, still get a pull request when a fix for a vulnerable dependency is available.
- The [Dependabot automerge workflow](https://github.com/ai-runway/airunway/blob/main/.github/workflows/dependabot-automerge.yml) enables auto-merge on a Dependabot pull request once CI and the backend, controller and gateway end-to-end workflows pass on its latest commit. Merging still follows the branch rules for `main`.
- Remove dependencies that are no longer used when the code that needed them goes away.

## Monitoring for vulnerabilities

- CI runs `govulncheck` against the controller and every provider module on each pull request and push to `main`. It reports findings without failing the build.
- GitHub Dependabot alerts are enabled for the repository.
- [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/ai-runway/airunway) runs weekly and on every push to `main`.

Report a vulnerability in a dependency that affects AI Runway as described in the [security policy](https://github.com/ai-runway/airunway/blob/main/SECURITY.md).
