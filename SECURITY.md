# Security Policy

## Reporting a Vulnerability

We take security seriously at AI Runway. We appreciate your efforts to responsibly disclose your findings and will make every effort to acknowledge your contributions.

### How to Report

**Please do NOT report security vulnerabilities through public GitHub issues.**

Instead, please report them via [GitHub Security Advisories](https://github.com/ai-runway/airunway/security/advisories/new).

If you prefer email, you can send your report to the repository maintainers. Please include the word "SECURITY" in the subject line.

You should receive a response within **72 hours**. If for some reason you do not, please follow up to ensure we received your original message.

### What to Include

Please include as much of the following information as possible to help us better understand the nature and scope of the issue:

- **Type of issue** (e.g., unauthorized access, privilege escalation, code injection, data exposure, etc.)
- **Full paths of source file(s)** related to the manifestation of the issue
- **Location of the affected source code** (tag/branch/commit or direct URL)
- **Any special configuration** required to reproduce the issue
- **Step-by-step instructions** to reproduce the issue
- **Proof-of-concept or exploit code** (if possible)
- **Impact of the issue**, including how an attacker might exploit it

### What to Expect

- **Acknowledgment**: We will acknowledge receipt of your vulnerability report within 72 hours.
- **Triage**: We will confirm the problem and determine the affected versions within 7 days.
- **Fix & Disclosure**: We will work on a fix and coordinate disclosure timing with you. We aim to release patches within 30 days of triage confirmation.
- **Credit**: We will credit reporters in the advisory (unless you prefer to remain anonymous).

## Disclosure Policy

We follow a **coordinated disclosure** model:

1. The reporter submits the vulnerability privately.
2. We confirm and triage the vulnerability.
3. We develop and test a fix.
4. We release the fix and publish a security advisory.
5. We publicly disclose the vulnerability details after the fix is available.

We ask that you:

- Allow us a reasonable amount of time to address the issue before public disclosure.
- Make a good faith effort to avoid privacy violations, destruction of data, and interruption or degradation of our services.
- Do not access or modify other users' data.

## Security Updates & Advisories

Security advisories will be published via [GitHub Security Advisories](https://github.com/ai-runway/airunway/security/advisories).

## Scope

The following are considered in-scope for security reports:

- **Controller vulnerabilities**: Issues in the Kubernetes controller that could allow unintended resource creation, modification, deletion, or privilege changes
- **API and dashboard vulnerabilities**: Authentication, authorization, input validation, or injection issues in the backend API, web UI, or Headlamp plugin
- **Custom resource validation bypasses**: Ways to bypass validation for `ModelDeployment`, `InferenceProviderConfig`, or related resources that could cause unsafe deployments
- **Provider integration issues**: Vulnerabilities in AI Runway provider integrations that could expose credentials, model data, or cluster resources
- **Denial of service**: Resource exhaustion attacks that bypass configured limits or disrupt deployments managed by AI Runway
- **Supply chain attacks**: Vulnerabilities in dependencies, build artifacts, or release assets that affect AI Runway

The following are generally **out of scope**:

- Issues in third-party inference providers, models, or Kubernetes distributions that are not caused by AI Runway (report to the respective maintainers)
- Social engineering attacks
- Denial of service through normal usage within configured resource limits

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| latest  | :white_check_mark: |

We recommend always running the latest version of AI Runway to benefit from the most recent security patches.

## Security Best Practices for Users

- **Pin dependencies**: Use lockfiles to ensure reproducible builds.
- **Review generated manifests**: Inspect generated deployment manifests before applying them to shared or production clusters.
- **Use least-privilege credentials**: Grant AI Runway only the cluster permissions required for the providers and namespaces you use.
- **Keep updated**: Regularly update AI Runway to receive security fixes.
- **Review provider access**: Only configure inference providers and model sources that you trust.

## References

This security policy is based on the [CNCF TAG Security project resources templates](https://github.com/cncf/tag-security/tree/main/community/resources/project-resources/templates).
