# Resource hub: per-peer manifest endpoint

**Status:** accepted (2026-11-05, operator-approved)

## Context

One operator runs many assistants. Every new assistant needs the same external
API keys, MCP server configs, skills and prompt snippets, and today each one is
configured by hand. The relay already knows who every peer is and already has a
proven distribution path for versioned content (prompt revisions, §8.6:
heartbeat nudge → pull → confirm in the next heartbeat).

DESIGN §1.3 keeps MCP out of scope as a *tool-access* layer. That stays true:
the relay never proxies MCP calls or runs skills. What it gains is a place to
**register a resource once, grant it to peers, and distribute the config**.

## Decision

Add one agent-authenticated, read-only endpoint, `GET /resources/current`,
backed by the resource hub tables (`resources`, `resource_revisions`,
`resource_grants`, DESIGN §4.12). It returns the calling peer's manifest:

- only resources granted to this peer, either directly or through the `*` grant;
- `secret` and `skill` entries carry a `fetch_url` instead of content (ADR 0005);
  `mcp` and `prompt` entries carry their content inline, with any
  `${secret:NAME}` references left **unresolved**;
- `manifest_rev`, a SHA-256 over the sorted `kind:name:content_hash` lines.
  A secret's `content_hash` is an HMAC under the hub key, so the digest never
  exposes a low-entropy secret to offline guessing.

Heartbeat carries the same digest in both directions, mirroring §8.6:

- request: `resources_rev`, the digest the assistant last applied (empty = not
  reporting, never clobbers the stored value);
- response: `resources_update=true` plus `resources_rev` when the current
  digest differs from the stored one. Peers with no grants get no nudge.

## Consequences

- The agent read surface grows by one endpoint; no new write path. All message
  writes still go through `POST /messages`.
- Grant changes, revocations and secret rotations all change the digest, so
  they all surface as one nudge. Revocation cannot erase copies already on the
  assistant's host; the console says so and recommends rotating upstream.
- Old receivers ignore the new heartbeat fields. Receivers learn to forward
  `resources_update` in a later, separate release (it changes `receiver_rev`
  and therefore triggers one legitimate fleet-wide `client_update`).
