# Resource hub: self-service registration

**Status:** accepted (2026-11-06, operator-approved)

## Context

ADR 0004/0005 made the resource hub operator-authored and read-only for
assistants. In practice the operator already has many API tokens, MCP configs
and skills living on individual assistant hosts. Re-typing them in the console
is error-prone, and an assistant is often the only place that knows the exact
runtime shape of its local MCP config or skill archive.

The hub therefore needs a controlled write path for peers, without turning
every peer into a trusted publisher: peer-authored MCP configs and skills are
executable content on other assistants, and a secret uploaded by one peer may
be redistributed to others.

## Decision

Add assistant-authenticated, write-only **submission** endpoints:

- `POST /resources/submissions` for `secret`, `mcp` and `prompt` JSON bodies;
- `POST /resources/submissions/skill` for a multipart `tar.gz`;
- `GET /resources/submissions/mine` and `DELETE /resources/submissions/{id}`
  for status/withdrawal;
- `POST /resources/takedowns` and `GET /resources/takedowns/mine` for owners
  asking the operator to remove a live resource.

Submissions are staged in `resource_submissions` with status
`pending|approved|rejected|withdrawn|expired|superseded`. They never appear in
`GET /resources/current` and never move `manifest_rev` until an operator
approves them. Approval is an explicit console action (`/admin/resource-submissions*`)
that creates a normal `resource_revisions` row and live `resources` pointer.
Registration never grants the resource to anyone, including its source peer.

Ownership and trust rules:

- resource names remain globally unique per `(kind,name)`; `resources.owner_peer`
  is `""` for operator-owned resources and the source peer id otherwise;
- a peer cannot overwrite another owner's resource or an operator resource;
- approvals may rename non-skill resources (metadata only). Skill names come
  from `SKILL.md`; a skill name conflict must be rejected and the source asked
  to change its `SKILL.md`;
- every content revision staged by a peer is reviewed again, including updates
  to an already-published resource; the old live revision keeps serving until
  approval;
- secret values are AES-GCM encrypted before they touch the database and are
  never returned by any admin API (the existing ADR 0005 invariant);
- rejected/withdrawn/expired submissions wipe their content, keeping only
  metadata, hash and the review note.

Limits are operator-configurable: 20 pending submissions and 100 live owned
resources per peer, 10 submissions per hour, 30-day pending TTL, and a global
plus per-peer enable switch.

## Consequences

- ADR 0004's "no new write path" is amended only for resource submissions.
  `POST /messages` remains the only message write path; the relay still never
  proxies MCP calls or executes skills.
- The operator becomes the review boundary for peer-authored executable
  content. The console must show skill file listings and `SKILL.md`, but must
  never show secret plaintext.
- A new `resource_submissions` table, `resource_takedowns` table,
  `resources.owner_peer` column and `peers.resource_submit_enabled` column are
  added; existing operator resources are owner `""`.
- Prompt version 19 teaches assistants the submission flow. No receiver
  change is needed: the endpoints are plain HTTP calls from the assistant and
  old receivers simply never use them.
