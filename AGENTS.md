# Project instructions

## Online diagnostics

- The repository-root `online.env` contains the deployed instance URL (`URL`) and API bearer token (`TOKEN`). Read these values at runtime; do not copy their contents into source, documentation, logs, screenshots, shell arguments, or commits.
- `online.env` is local configuration and must remain ignored by Git. The Docker build context also excludes it through the existing allowlist in `.dockerignore`.
- Send the token in the HTTP header `Authorization: Bearer <TOKEN>`, never in the query string. Resolve API paths relative to `URL`.
- Start with read-only requests to `/api/diagnostics/status`, `/api/diagnostics/metrics`, `/api/diagnostics/logs`, and `/api/diagnostics/activity`. A diagnostics-scoped token cannot access `/api/admin/*`; respect that boundary and use the returned metrics/runtime logs to investigate page performance.
- Last verified on 2026-11-01: the supplied token could read diagnostic status/metrics/logs; administrator API requests returned 401. The deployed service reported v1.0.37. Recheck current state rather than assuming this version or permission remains unchanged.
- The tasks page is `/admin/tasks`; the logs page is `/admin/logs`. Verify their current API requests from the frontend source before measuring. With an administrator token, authenticated GET requests to those APIs are available as well.
- A request to diagnose online performance authorizes read-only investigation. Do not change online settings, start/cancel/retry jobs, clear history, restart the service, or deploy changes unless the user asks for those actions.
- Keep diagnostic evidence under the ignored `audit/` directory and redact credentials and private instance data from anything shared.
