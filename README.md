# daiki-ai-passport-backend

Lightweight Go/Chi backend for Daiki AI Passport.

## Responsibilities
- Validate Keycloak OIDC access tokens and enforce RBAC.
- Persist Daiki application users separately from Keycloak identity and enforce `pending` / `approved` / `suspended` / `rejected` access state.
- Proxy chat/model requests to LiteLLM without exposing its master key to Vercel/browser clients.
- Stream LLM responses over SSE-compatible HTTP.
- Keep Keycloak administrative identity creation server-side for `ai-admin` users.
- Issue Daiki-owned API keys. Plaintext keys are returned once; only a bounded prefix and SHA-256 hash are stored. API keys cannot call admin APIs and must carry explicit scopes such as `inference`.
- Enforce Admin-controlled token entitlements before inference. PostgreSQL is the durable usage/policy ledger; Redis tracks atomic in-flight reservations.
- Expose liveness, readiness and Prometheus metrics.

## Main routes
- `GET /v1/health/live`
- `GET /v1/health/ready`
- `GET /v1/me`
- `GET /v1/models`
- `GET /v1/usage`
- `POST /v1/chat`
- `POST /v1/chat/stream`
- `GET|POST /v1/admin/users`
- `PATCH /v1/admin/users/{subject}/status`
- `GET|PUT /v1/admin/users/{subject}/quota`
- `GET|POST /v1/admin/api-keys`
- `DELETE /v1/admin/api-keys/{id}`
- `GET|PUT /v1/admin/api-keys/{id}/quota`
- `GET /v1/admin/summary`
- `GET /metrics`

Local LLM is intentionally optional until the dedicated GPU host is connected. Platform readiness depends on Redis/PostgreSQL, not on the LLM upstream.

## Current implementation boundary

The whitelist, Daiki API-key and token-entitlement path is implemented in code. The initial Redis-backed distributed inference queue and stable model-alias router are also implemented, with global/per-principal/workload concurrency, priority, leases, bounded waiting and cancellation. Complete token extraction from streaming responses, capable vision/image providers, Google-provider attribution, files/projects/skills/MCP and Local LLM integration remain later roadmap work. A successful local build is not evidence that the match-infra deployment is live.
