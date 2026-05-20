# AI Runway Project Governance

This document defines the governance structure for the AI Runway project. It describes how decisions are made, how contributors advance through the project, and how conflicts are resolved.

This governance model is inspired by [CNCF project governance examples](https://contribute.cncf.io/maintainers/governance/) and follows principles of openness, transparency, and meritocracy.

## Principles

- **Open**: AI Runway is open source and operates transparently. All decisions, discussions, and artifacts are publicly accessible.
- **Welcoming and respectful**: We follow the [CNCF Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md).
- **Merit-based**: Advancement is based on the quality and impact of contributions, not on corporate affiliation.
- **Transparent**: Proposals, decisions, and the rationale behind them are publicly documented.

## Project Roles

AI Runway defines a contributor ladder with four levels. Each role builds on the responsibilities of the previous one.

### Community Participant

Anyone who interacts with the project — filing issues, asking questions, participating in discussions, or using AI Runway.

**Responsibilities:**
- Follow the [Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md)
- Be respectful in all interactions

**Privileges:**
- Can open issues and pull requests
- Can participate in discussions
- Can submit prompt requests (see [CONTRIBUTING.md](CONTRIBUTING.md))

---

### Contributor

A community participant who has made meaningful contributions to the project.

**Requirements (any of):**
- Has had at least one non-trivial PR merged
- Has authored or substantially improved documentation
- Has contributed meaningful issue triage or code reviews
- Has submitted accepted prompt requests that resulted in merged code

**Defined by:** Listed in `CONTRIBUTORS.md` or via GitHub contributor history.

**Responsibilities:**
- Continue making quality contributions
- Follow coding standards and conventions ([docs/standards.md](docs/standards.md))
- Respond to feedback on their PRs

**Privileges:**
- May be assigned issues
- May be requested as a reviewer on PRs in their area of expertise
- Recognized in project communications

---

### Reviewer

A contributor who has demonstrated sustained commitment and deep knowledge of one or more project areas.

**Requirements (all of):**
- Has been a Contributor for at least 2 months
- Has authored at least 5 substantial PRs in the relevant area
- Has reviewed at least 5 PRs from other contributors
- Demonstrates understanding of project architecture and design principles
- Nominated by a Maintainer and approved by lazy consensus of existing Maintainers

**Defined by:** Listed in `MAINTAINERS.md` under the Reviewers section with their area(s) of expertise.

**Responsibilities:**
- Review PRs in their designated area(s) promptly (target: within 3 business days)
- Provide constructive, actionable feedback
- Help contributors understand project conventions
- Triage issues in their area
- Mentor new contributors

**Privileges:**
- Listed as a code owner for their area (`.github/CODEOWNERS`)
- Can approve PRs in their area (approval is required but not sufficient for merge)
- May attend Maintainer meetings as observers
- Can request promotion to Maintainer

---

### Maintainer

A Reviewer who has demonstrated broad project knowledge, good judgment on design decisions, and commitment to the project's long-term health.

**Requirements (all of):**
- Has been a Reviewer for at least 3 months
- Has authored or reviewed PRs across multiple project areas
- Demonstrates sound judgment in architectural and design discussions
- Has shown commitment to the project's long-term sustainability
- Nominated by an existing Maintainer and approved by supermajority (2/3) vote of current Maintainers

**Defined by:** Listed in `MAINTAINERS.md` under the Maintainers section. Has merge rights and GitHub `maintain` or `admin` role.

**Responsibilities:**
- Set technical direction and roadmap
- Review and merge PRs (ensuring CI passes and at least one approval exists)
- Manage releases and release branches
- Ensure project health (CI/CD, dependencies, security)
- Participate in governance decisions
- Mentor Reviewers and Contributors
- Represent the project in external forums
- Respond to security reports promptly

**Privileges:**
- Merge access to all repositories
- Can vote on governance decisions
- Can nominate and vote on new Reviewers and Maintainers
- Can participate in private security discussions
- Listed in `SECURITY.md` as a security contact

---

## Decision-Making

### Lazy Consensus

Most decisions in AI Runway are made through **lazy consensus**. A proposal is considered accepted if:

1. The proposal is publicly posted (as a PR, issue, or discussion)
2. A reasonable waiting period has passed (see below)
3. No Maintainer has raised a blocking objection

**Waiting periods:**
| Decision Type | Minimum Wait | Where |
|---|---|---|
| Code changes (bug fixes, small features) | 1 business day | Pull Request |
| Documentation changes | 1 business day | Pull Request |
| New features (significant) | 5 business days | GitHub Discussion or Issue |
| Architecture/design changes | 7 business days | GitHub Discussion + Design Doc |
| Governance changes | 10 business days | Pull Request to GOVERNANCE.md |
| Adding/removing Maintainers | 7 business days | Private Maintainer discussion |

A **blocking objection** must include:
- A clear explanation of the concern
- A constructive alternative or condition for acceptance
- Willingness to engage in resolution

Silence is consent. If no objection is raised within the waiting period, the proposal is accepted.

### Formal Voting

Formal votes are required for:
- Adding or removing Maintainers
- Changes to governance documents
- Licensing changes
- Decisions where lazy consensus fails (after good-faith attempts at resolution)

**Voting rules:**
- Each Maintainer has one vote, regardless of employer
- Votes are cast publicly in a GitHub issue or discussion (except for sensitive personnel matters)
- Voting period: 7 calendar days from the call for votes
- Quorum: 2/3 of active Maintainers must participate
- Approval threshold:
  - **Simple majority (>50%)**: Routine decisions, adding Reviewers
  - **Supermajority (≥2/3)**: Adding/removing Maintainers, governance changes, licensing changes
- Abstentions count toward quorum but not toward the approval threshold

**Active Maintainer** is defined as a Maintainer who has contributed (code, review, or governance participation) within the last 6 months.

### Design Proposals

Significant changes require a lightweight design proposal:

1. Open a GitHub Discussion with the `proposal` label
2. Include: motivation, proposed design, alternatives considered, and migration path
3. Allow the waiting period for feedback
4. If no blocking objections, implement via PR(s)
5. If objections arise, iterate on the design or escalate to a vote

---

## Conflict Resolution

### Technical Disagreements

1. **Discussion**: Attempt to resolve through PR comments or GitHub Discussions
2. **Mediation**: If unresolved after 5 business days, any party may request mediation by a Maintainer not involved in the dispute
3. **Maintainer Vote**: If mediation fails, the issue is brought to a Maintainer vote (simple majority)
4. **External Mediation**: If the vote is contested or involves a governance issue, the project may seek guidance from the CNCF TOC (if applicable) or a mutually agreed neutral party

### Code of Conduct Violations

Code of Conduct violations are handled separately from technical disagreements:

1. Reports are sent to the project's Code of Conduct committee (all Maintainers, or a designated subset)
2. The committee investigates privately and confidentially
3. Actions may include: warning, temporary ban, permanent ban
4. Appeals may be directed to the CNCF Code of Conduct Committee

### Maintainer Removal

A Maintainer may be removed for:
- Inactivity (no contributions for 12 months without prior notice)
- Repeated Code of Conduct violations
- Actions that harm the project's health or reputation

**Process:**
1. A Maintainer raises the concern privately with other Maintainers
2. The affected Maintainer is given the opportunity to respond
3. A vote is held (supermajority required for involuntary removal)
4. Voluntary step-down is always possible and honored gracefully

---

## Maintainer Inactivity and Emeritus Status

Maintainers who cannot continue active participation are encouraged to move to **Emeritus** status voluntarily. Emeritus Maintainers:

- Are recognized for their past contributions
- Listed in `MAINTAINERS.md` under the Emeritus section
- Do not have merge access or voting rights
- May return to active status by following the standard nomination process (with a reduced waiting period at the discretion of active Maintainers)

If a Maintainer is inactive for **12 months** without explanation, the remaining Maintainers will:
1. Attempt to contact them
2. Wait 30 days for a response
3. If no response, move them to Emeritus status via supermajority vote

---

## Meetings

- **Maintainer Sync**: Held as needed (minimum monthly) to discuss roadmap, governance, and escalated issues
- **Community Meeting**: Open to all, held monthly (schedule posted in README or Discussions)
- Meeting notes are published publicly in GitHub Discussions

---

## Project Assets

Maintainers collectively manage:
- GitHub organization and repository settings
- CI/CD infrastructure
- Release signing keys
- Domain names and hosting
- Social media accounts
- Container registry accounts

No single Maintainer should be the sole owner of critical project infrastructure. All credentials must be accessible to at least 2 Maintainers.

---

## Amendments

This governance document may be amended by:
1. Opening a PR with proposed changes
2. Allowing a 10 business day comment period
3. Approval by supermajority (≥2/3) vote of active Maintainers

---

## References

- [CNCF Governance Templates](https://contribute.cncf.io/maintainers/governance/)
- [Kubernetes Governance](https://github.com/kubernetes/community/blob/master/governance.md)
- [Helm Governance](https://github.com/helm/community/blob/main/governance/governance.md)
- [CNCF Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md)
- [AI Runway Contributing Guide](CONTRIBUTING.md)
