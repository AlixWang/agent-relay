# Agent runtime logs use a dedicated write path

**Status:** accepted (2026-11-03, operator-approved)

## Context

The relay needs the local delivery chain's diagnostics — relay-tail connection
state, relay-watch spool/wake decisions and worker start/timeout — to be visible
from the operations console. These records are high-volume, short-lived
operational data, not messages and not security audit entries.

Putting them on `POST /messages` would mix diagnostics into the delivery queue,
pollute thread/fuse/ack semantics and make a diagnostic upload look like an
instruction. Putting them in `audit_log` would bury the low-volume security trail
under high-volume client noise. The closest existing alternative, an unbounded
log file visible only on the agent host, is exactly the gap this change closes.

## Decision

Add one agent-authenticated, best-effort write endpoint, `POST /logs`, backed by
the dedicated `agent_logs` table (DESIGN §4.11). It is a diagnostics sink, not a
message delivery path:

- `peer_id` comes from the bearer token, never from the request body;
- the endpoint never accepts a recipient, thread routing, ack or task execution;
- batches are bounded, validated, rate-limited and sanitized before storage;
- upload failure must never block `/messages`, `/ack` or `wake()`;
- retention and per-peer row caps apply separately from `audit_log`.

The console read endpoints `GET /admin/logs` and `GET /admin/logs/export` are
admin-authenticated and read-only.

`POST /logs` deliberately skips the §8.5 `min_client` gate: a client that is
too old to use the current protocol must still be able to report why it cannot
connect. This is the only protocol endpoint with that exemption; the reason is
recorded in DESIGN §4.11 and §8.5.

## Consequences

- The agent-facing write path grows by exactly one endpoint, and it is not a
  second message-delivery channel; all message writes still go through
  `POST /messages`.
- `internal/agentlog` owns validation, sanitization and rate limiting; the
  gateway owns auth and HTTP status mapping; the store owns retention.
- Existing clients need to download the new `relay-watch.sh` to report; old
  clients remain fully functional and simply produce no agent logs.
- If a future feature needs to read logs from the agent side, that is a separate
  read-only endpoint and requires its own ADR.
