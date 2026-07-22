# KubeAIRunway Hub: Multi-Instance Portal with OAuth

## Problem Statement

Infra admins deploy KubeAIRunway to Kubernetes clusters but need to give users access without sharing kubeconfigs. Users should log in with Azure Entra ID or GitHub credentials, see available KubeAIRunway instances they have access to, and one-click deploy models — all without ever touching cluster credentials.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                     KubeAIRunway Hub (Portal)                   │
│                                                                 │
│  ┌──────────┐  ┌──────────┐  ┌──────────────┐  ┌────────────┐  │
│  │  OAuth    │  │  Instance │  │  RBAC        │  │  Cluster   │  │
│  │  (Entra/  │  │  Registry │  │  (Users →    │  │  Proxy     │  │
│  │  GitHub)  │  │          │  │   Instances)  │  │            │  │
│  └──────────┘  └──────────┘  └──────────────┘  └─────┬──────┘  │
│                                                       │         │
│  ┌──────────────────────┐  ┌──────────────────────┐   │         │
│  │   PostgreSQL         │  │  Azure Key Vault     │   │         │
│  │   (Users, Roles,     │  │  (Cluster creds via  │◄──┘         │
│  │    Sessions, Groups) │  │   Secrets Store CSI) │             │
│  └──────────────────────┘  └──────────────────────┘             │
└─────────────────────────────────────────────────────────────────┘
        │                           │               │
        ▼                           ▼               ▼
  ┌──────────┐              ┌──────────┐     ┌──────────┐
  │ K8s      │              │ K8s      │     │ K8s      │
  │ Cluster A│              │ Cluster B│     │ Cluster C│
  │ (KAITO)  │              │ (Dynamo) │     │ (KubeRay)│
  └──────────┘              └──────────┘     └──────────┘
```

## Key Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Architecture | Extend existing KubeAIRunway app | Greenfield — no backward compat needed |
| Auth providers | Azure Entra ID + GitHub (admin-configurable) | Enterprise + developer coverage |
| Credential storage | Azure Key Vault via Secrets Store CSI | Secrets never in app DB; auto-rotation support |
| App database | PostgreSQL | HA-ready, multi-replica support |
| Access model | Role-based with namespace isolation | Admin assigns users/groups to instances + namespaces |
| Group sync | Azure Entra security groups | Automatic access mapping, managed in Entra |
| Credential rotation | Auto-refresh via CSI volume watch | No pod restart needed |

## Scope

### In-Scope (v1)
- OAuth login (Azure Entra ID + GitHub, admin-configurable)
- Central portal as proxy (users never see cluster credentials)
- Instance registration with Azure Key Vault credentials
- Role-based access: users/groups → instances + namespaces
- Azure Entra group sync for automatic access mapping (with overage claim handling)
- GitHub org/team membership sync for group-based access
- Instance health dashboard (GPU capacity, status, deployments)
- Namespace isolation per user/group
- One-click model deployment (select instance → deploy)
- Auto-refresh of rotated credentials (CSI volume watch)
- Audit logging (all user actions to PostgreSQL) — required for multi-tenant access proxy
- API path allowlist for cluster proxy — only KubeAIRunway-specific paths forwarded

### Future Work (noted, not implemented)
- API tokens / personal access tokens for CLI/CI-CD
- GPU quotas per namespace/user
- Cost visibility / chargeback per user

---

## Implementation Plan

### Phase 1: Database & Data Model

**1.1 PostgreSQL schema and ORM setup**
- Add PostgreSQL client library (e.g., `drizzle-orm` + `pg` or `postgres` package)
- Design and create database schema:
  - `users` — id, email, display_name, provider (entra/github), provider_id, created_at, last_login
  - `sessions` — id, user_id, token_hash, expires_at, created_at
  - `instances` — id, name, display_name, endpoint_url, credential_ref (path to CSI-mounted secret), status, created_at
  - `roles` — id, name (admin, deployer, viewer)
  - `user_instance_roles` — user_id, instance_id, role_id, namespaces (JSON array of allowed namespaces)
  - `entra_group_mappings` — entra_group_id, entra_group_name, instance_id, role_id, namespaces
  - `oauth_providers` — id, type (entra/github), enabled, client_id, tenant_id (for Entra), client_secret_ref
- Create migration system (drizzle-kit or manual SQL migrations)
- Add `DATABASE_URL` environment variable

**1.2 Database service layer**
- Create `backend/src/services/database.ts` — connection pool, query helpers
- Create repository classes: UserRepository, InstanceRepository, RoleRepository, SessionRepository
- Unit tests for data layer

### Phase 2: OAuth Authentication

**2.1 OAuth provider framework**
- Create `backend/src/services/oauth/` directory with provider interface
- Define `OAuthProvider` interface: `getAuthUrl()`, `exchangeCode()`, `getUserInfo()`, `refreshToken()`
- Implement Azure Entra ID provider:
  - OIDC discovery (`https://login.microsoftonline.com/{tenant}/v2.0/.well-known/openid-configuration`)
  - Authorization code + PKCE flow
  - Token exchange, refresh token handling
  - Extract user info + group memberships from ID token `groups` claim
  - Handle group overage claim: when user belongs to >150 groups, Entra returns a `_claim_names`/`_claim_sources` overage indicator instead of the `groups` claim — detect this and fall back to Microsoft Graph API (`/me/memberOf`) to enumerate groups (see [Microsoft docs](https://learn.microsoft.com/en-us/troubleshoot/entra/entra-id/app-integration/get-signed-in-users-groups-in-access-token))
- Implement GitHub provider:
  - OAuth App flow with PKCE (authorization code + code_challenge, [GitHub PKCE support](https://github.blog/changelog/2025-07-14-pkce-support-for-oauth-and-github-app-authentication/))
  - Token exchange via `https://github.com/login/oauth/access_token`
  - User info from `https://api.github.com/user`; request `user:email` scope and call `GET /user/emails` to retrieve primary verified email (the `/user` endpoint email field is null when the user has set their email to private)
  - Org/team membership for group-based access (required for parity with Entra group sync): fetch via `GET /user/orgs` and `GET /user/teams` with `read:org` scope

**2.2 Auth routes and middleware**
- Create/update `backend/src/routes/auth.ts`:
  - `GET /api/auth/providers` — list enabled OAuth providers
  - `GET /api/auth/login/:provider` — initiate OAuth flow (redirect to provider)
  - `GET /api/auth/callback/:provider` — handle OAuth callback, create session
  - `POST /api/auth/logout` — invalidate session
  - `GET /api/auth/me` — get current user info + accessible instances
- Update auth middleware in `backend/src/hono-app.ts`:
  - Replace Kubernetes TokenReview with portal session validation
  - Attach user + permissions to request context
  - CSRF protection for OAuth flows

**2.3 Session management**
- JWT-based session tokens (short-lived access token + longer-lived refresh token)
- Store refresh tokens hashed in PostgreSQL `sessions` table
- Token refresh endpoint: `POST /api/auth/refresh`
- Automatic session cleanup (expired sessions)

### Phase 3: Instance Management

**3.1 Credential loading from Azure Key Vault (CSI)**
- Create `backend/src/services/credentials.ts`:
  - Read kubeconfig/SA token files from CSI-mounted volume path (configurable via `CREDENTIALS_PATH` env var)
  - Parse kubeconfig files into usable K8s client configs
  - File watcher (fs.watch) for auto-refresh when CSI driver rotates secrets
  - In-memory cache of parsed credentials, invalidated on file change
- Credential file mapping: use `instances.credential_ref` column to explicitly reference the CSI-mounted file path, rather than relying on filename = instance name convention. This decouples AKV secret naming from instance identity and enables reconciliation checks to detect missing or stale credential files

**3.2 Instance registry**
- Create `backend/src/routes/instances.ts`:
  - `GET /api/instances` — list instances accessible to current user (filtered by RBAC)
  - `GET /api/instances/:id` — get instance details + health status
  - `POST /api/instances` — register new instance (admin only)
  - `PUT /api/instances/:id` — update instance config (admin only)
  - `DELETE /api/instances/:id` — deregister instance (admin only)
  - `GET /api/instances/:id/health` — real-time health check (GPU capacity, node count, connection status)
- Create `backend/src/services/instance-manager.ts`:
  - Maintain K8s client per registered instance (from CSI-loaded credentials)
  - Health check loop (periodic connectivity + GPU capacity polling)
  - Instance status tracking

**3.3 Cluster proxy layer**
- Create `backend/src/services/cluster-proxy.ts`:
  - Accept requests with instance context (from user's session)
  - Validate user has access to target instance + namespace (RBAC check)
  - **API path allowlist**: only forward requests matching KubeAIRunway-specific paths (model deployments, status, health, metrics). Deny arbitrary K8s API paths (e.g., `/api/v1/secrets`, `/api/v1/configmaps`) to prevent credential over-privilege exploitation. The allowlist should cover:
    - ModelDeployment CRD operations
    - Namespace listing (filtered)
    - Node/GPU capacity queries
    - Pod logs for model workloads
    - Deny all other K8s API paths by default
  - Forward allowed API calls to target cluster's KubeAIRunway using stored credentials
  - Response mapping (add instance context to responses)
- Update existing routes (`deployments.ts`, `models.ts`, etc.) to be instance-aware:
  - Accept `instance_id` parameter
  - Route through cluster proxy instead of direct K8s client
  - Namespace filtering based on user permissions

### Phase 4: RBAC & Group Sync

**4.1 Role-based access control**
- Create `backend/src/services/rbac.ts`:
  - Roles: `admin` (full access, manage instances/users), `deployer` (deploy/delete models), `viewer` (read-only)
  - Permission checks: `canAccessInstance(user, instanceId)`, `canDeployToNamespace(user, instanceId, namespace)`
  - Middleware: inject RBAC context into request
- Admin routes for user management:
  - `GET /api/admin/users` — list all users
  - `POST /api/admin/users/:id/roles` — assign user to instance with role + namespaces
  - `DELETE /api/admin/users/:id/roles/:roleId` — revoke access
  - `GET /api/admin/group-mappings` — list Entra group mappings
  - `POST /api/admin/group-mappings` — create group-to-instance mapping
  - `DELETE /api/admin/group-mappings/:id` — delete mapping

**4.2 Azure Entra group sync**
- On user login (Entra), fetch group memberships from ID token `groups` claim
- Handle overage claim: if `_claim_names` contains `groups`, the token has too many groups — fetch full list via Microsoft Graph API `/me/memberOf` using the access token
- Match groups against `entra_group_mappings` table
- Auto-assign instance access based on matched groups
- Periodic background sync (optional) to handle group changes between logins

**4.3 GitHub org/team sync**
- On user login (GitHub), fetch org memberships via `GET /user/orgs` and team memberships via `GET /user/teams`
- Match orgs/teams against group mappings table (reuse `entra_group_mappings` schema or create unified `group_mappings` table)
- Auto-assign instance access based on matched orgs/teams
- Provides parity with Entra group sync for GitHub-authenticated users

### Phase 5: Frontend — Hub UI

**5.1 Login page redesign**
- Replace CLI-based login with OAuth buttons
- Show enabled providers (fetched from `GET /api/auth/providers`)
- "Sign in with Microsoft" and "Sign in with GitHub" buttons
- OAuth redirect flow → callback → session creation → redirect to dashboard
- Handle auth errors gracefully

**5.2 Instance selector / dashboard**
- New landing page: `InstancesPage.tsx` — grid of accessible instances
- Instance card component showing:
  - Instance name + cluster info
  - Connection status (green/yellow/red)
  - GPU capacity (used/total with progress bar)
  - Active deployments count
  - "Open" button to enter instance context
- Instance context: once selected, store in React state/URL, scope all subsequent views to that instance

**5.3 Instance-scoped views**
- Update existing pages to work within instance context:
  - `ModelsPage` — show models available on selected instance
  - `DeployPage` — deploy to selected instance (namespace picker filtered by user permissions)
  - `DeploymentsPage` — show deployments on selected instance (filtered by allowed namespaces)
  - `DeploymentDetailsPage` — scoped to instance
- Add instance breadcrumb/selector in the header/sidebar
- Namespace picker component (shows only user's allowed namespaces)

**5.4 Admin pages**
- `AdminPage.tsx` — admin-only section:
  - Instance management (register, edit, remove instances)
  - User management (list users, assign roles)
  - Group mappings (map Entra groups to instances/namespaces)
  - OAuth provider configuration
- Protected by `admin` role check

**5.5 Auth hooks and context updates**
- Rewrite `useAuth.ts` for OAuth flow (replace localStorage token with session-based auth)
- New `useInstances.ts` hook — fetch and manage instance list
- New `useInstanceContext.ts` hook — current instance selection
- Update `api.ts` to include instance context in all requests
- Update `ProtectedRoute` to check OAuth session + RBAC

### Phase 6: Deployment & Configuration

**6.1 Deployment manifests**
- Update Kubernetes deployment manifests for the portal:
  - Add PostgreSQL dependency (or reference external PostgreSQL)
  - Add Secrets Store CSI volume mounts for Azure Key Vault
  - Add SecretProviderClass CRD for Azure Key Vault integration
  - Environment variables for OAuth config, DATABASE_URL, CREDENTIALS_PATH
- Helm chart or Kustomize overlays for configurable deployment

**6.2 Configuration and environment**
- New environment variables:
  - `DATABASE_URL` — PostgreSQL connection string
  - `CREDENTIALS_PATH` — path to CSI-mounted credential files
  - `SESSION_SECRET` — JWT signing key
  - `AZURE_TENANT_ID` — Azure Entra tenant
  - `AZURE_CLIENT_ID` — Azure Entra app registration client ID
  - `AZURE_CLIENT_SECRET_REF` — reference to client secret in Key Vault
  - `GITHUB_CLIENT_ID` — GitHub OAuth App client ID
  - `GITHUB_CLIENT_SECRET_REF` — reference to client secret in Key Vault
  - `ENABLED_AUTH_PROVIDERS` — comma-separated list (e.g., `entra,github`)
- Documentation for Azure Entra app registration setup
- Documentation for GitHub OAuth App setup

**6.3 Credential auto-refresh**
- File system watcher on `CREDENTIALS_PATH` directory
- On file change: reload and re-parse credentials, update K8s client
- Graceful handling of temporary file unavailability during rotation
- Logging of credential refresh events

---

## Technical Notes

### Security Considerations
- OAuth state parameter to prevent CSRF
- PKCE for all OAuth flows (both Entra and GitHub)
- Session tokens are httpOnly, secure, sameSite cookies (not localStorage)
- Cluster credentials never exposed to frontend or API responses
- All proxy requests validated against RBAC before forwarding
- API path allowlist on cluster proxy — only KubeAIRunway-specific K8s API paths are forwarded; arbitrary paths (secrets, configmaps, etc.) are denied even if stored credentials have broad permissions
- Rate limiting on auth endpoints
- Audit logging of all user actions (login, deploy, delete, admin operations) for multi-tenant accountability

### Database Migrations
- Use a migration tool (drizzle-kit) for schema versioning
- Migrations run automatically on startup
- Rollback support for failed migrations

### Audit Logging
- Log all user actions (login, logout, deploy, delete, admin operations) to PostgreSQL `audit_log` table
- Schema: `id`, `user_id`, `action`, `resource_type`, `resource_id`, `instance_id`, `details` (JSON), `ip_address`, `created_at`
- Required for multi-tenant access proxy compliance and incident investigation
- Query-friendly for admin dashboards and security reviews

### Testing Strategy
- Unit tests: OAuth providers (including overage claim handling, private email fallback), RBAC logic, credential parsing, API path allowlist
- Integration tests: OAuth flow (mocked providers), cluster proxy (verify blocked paths), session management
- E2E tests: Login → select instance → deploy model flow
