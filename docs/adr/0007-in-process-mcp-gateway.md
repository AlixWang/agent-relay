# In-process MCP gateway

**Status:** accepted (2026-11-25, operator-approved; amends DESIGN §1.3, ADR-0004, and ADR-0006)

## Context

`DESIGN §1.3` and ADR-0004/0006 positioned the relay's Resource Hub (`§4.12`) strictly as a distributor of static MCP configs and skills to assistants, never proxying MCP tool calls. In practice, distributing raw third-party credentials (such as Google OAuth2 `authorized_user` ADC refresh tokens and Bing Webmaster API keys) to every assistant host or external MCP client breaks least privilege:

- Every host holding the raw secret can invoke any upstream read or destructive write operation without per-tool scoping.
- There is no central human-in-the-loop approval gate for high-risk writes, no uniform per-principal rate limiting, and no unified audit trail.
- Credential rotation requires re-syncing every client rather than invalidating one server-side reference.

## Decision

Add an opt-in, in-process MCP gateway (`internal/mcpgateway/`) alongside the existing Resource Hub:

- **Endpoints & transport:** Mounted at `GET /mcp/health` (unauthenticated status), `POST /mcp` (aggregate endpoint using prefixed tool names such as `adsense_*`, `bing_*`, `ga_*`), and `POST /mcp/{service}` (per-service endpoints using short tool names). Implements Streamable HTTP (`2025-06-18` / `2025-03-26`), stateless JSON-only. `GET`/`DELETE` on RPC paths return `405`; `Origin` is validated against `Host` / loopback to block DNS rebinding; bodies are bounded by `max_body_bytes`; when `[mcp_gateway].enabled = false`, `POST /mcp*` returns `503` immediately before reading the body or writing audit rows.
- **Dual authentication & scoping:** Accepts either relay assistant Bearer tokens (authorized by active `mcp_keys` peer grant rows, requiring `status = "active"` and `protocol_version >= min_client` or returning `426`) or operator-issued `rmk_` keys (SHA-256 hashed at rest, plaintext returned once on creation/rotation, read-only by default, atomic rotation with optional grace period). Scopes follow `mcp:<service>:<read|write>:<tool|*>` (per-service wildcards only). Pruned or deleted peers automatically lose their `mcp_keys` peer grant inside the peer cleanup transaction.
- **Four-layer write gate & second approval:** Writes require all four layers: `[mcp_gateway].enabled`, `[mcp_gateway.write].enabled`, explicit listing in `[mcp_gateway.write.<service>].allowed_tools`, and caller scope. High-risk writes (such as `bing-webmaster` `add_blocked_url` and `remove_blocked_url`) enter a CAS approval workflow: `pending` (24h default TTL) → `approved | rejected | expired` → `executed` (10m default execute window, one-shot CAS before upstream I/O). Request parameters are strictly checked against duplicate or case-colliding JSON keys, frozen canonically as `(service_id, tool, params)` with SHA-256, and executed strictly from the frozen `params_json`.
- **In-process secret resolution & upstream isolation:** Services resolve `secret:<name>` references through the Resource Hub in memory (plaintext credentials are disallowed in config; secret updates/deletes invalidate the gateway cache). Upstream HTTP clients disable redirects (`http.ErrUseLastResponse`) to prevent credential or OAuth form replay. AdSense and GA share a serialized Google OAuth2 ADC token provider (`internal/mcpgateway/google_adc.go`) with SHA-256 secret-change detection and `401`-only single retry.
- **Initial service catalog:**
  - `adsense`: 9 read tools; 5 write placeholders remain unexposed (`not_implemented`) until AFP approval and full-scope ADC credentials are available. `generate_report` additionally supports `format=csv` (upstream `reports:generateCsv`), `offset` paging and the official OS/browser/webview/traffic-source dimensions as parameters (no new tools).
  - `bing-webmaster`: 23 read tools + 6 write tools (4 low-risk direct; `add_blocked_url` and `remove_blocked_url` high-risk via second approval).
  - `ga`: 10 read tools reusing the same ADC secret as AdSense, including `list_key_events` (Admin API v1beta `keyEvents.list`); GA writes (`create_property_annotation`) are out of scope.
- **Persistence & audit:** Adds `mcp_keys`, `mcp_approvals`, and `mcp_audit` to both `internal/store/schema.sql` and `migrations/001_init.sql` (following repo conventions: no foreign keys or `CHECK` constraints, Unix-second integer timestamps). Gateway calls and approval transitions are audited in `mcp_audit` (retained 90 days for reads, 180 days for writes, capped at 2M rows) and mirrored best-effort into `audit_log`; CSV export neutralizes spreadsheet formula prefixes.

## Consequences

- `DESIGN §1.3`, ADR-0004, and ADR-0006 are amended strictly for explicitly registered, opt-in in-process MCP gateway services.
- The Resource Hub (`§4.12`) still never executes skills, and `POST /messages` remains the sole message write path.
- Both `[mcp_gateway].enabled` and `[mcp_gateway.write].enabled` default to `false` so existing deployments are unaffected unless explicitly configured.

## Amendments

- **2026-11-27 — P2/P3 read expansion (48 tools):** AdSense stays at 9 tools but `generate_report` gains `format=csv` (`reports:generateCsv`), an explicit `offset` paging window and the official `OS_*`/`BROWSER_*`/`WEBVIEW_*`/`TRAFFIC_SOURCE_*` dimensions; the two non-official `WEBSEARCH_RESULT_TYPE_*` names were removed. Bing gained seven long-tail reads (`get_query_page_detail_stats`, `get_query_traffic_stats`, `get_children_url_info`, `get_children_url_traffic_info`, `get_link_counts`, `get_url_links`, `get_feed_details`), taking it to 23 read + 6 write. GA gained `list_key_events` (Admin API v1beta `properties.keyEvents.list`), taking it to 10 reads. Total 40 → 48; no write tool or write allowlist changed. `run_realtime_report` does not gain `offset`: the official Data API v1beta `RunRealtimeReportRequest` (discovery document and googleapis proto) has no offset field, and the upstream analytics-mcp 1.1.0 signature that advertises one is backed by a client whose proto has no such field; the gateway therefore rejects the parameter instead of sending an invalid request.
- **2026-11-27 — Runtime console controls & live credential activation:** While `config.toml` continues to provide startup defaults and safety ceilings, the operator console (`PUT /admin/mcp/config`, `DELETE /admin/mcp/config`, `PUT /admin/mcp/services/{id}`) can toggle the gateway master switch, write master switch, per-service enablement, and per-service read/write tool allowlists at runtime (persisted in SQLite `settings` under `mcp.runtime_policy_v1` without a schema change; explicit write tool names remain mandatory and `"*"` is still forbidden). Missing `secret:<name>` credentials no longer permanently disable the service segment at boot: `/admin/mcp/services` surfaces live configuration status (`credential_configured`), the MCP Services UI allows configuring/updating the Resource Hub encrypted secret directly, and saving the secret invalidates the gateway cache so the service activates immediately without restarting.
