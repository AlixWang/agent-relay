# Resource hub: per-resource fetch endpoints

**Status:** accepted (2026-11-05, operator-approved)

## Context

The manifest (ADR 0004) deliberately omits secret values and skill archives:
secrets must be audited per read and must never sit in a cacheable listing,
and skill archives (up to 1 MiB) would bloat every manifest pull. MCP configs
reference secrets as `${secret:NAME}`; an assistant that only has `curl` still
needs a ready-to-use config.

## Decision

Add three agent-authenticated, read-only endpoints:

- `GET /resources/secrets/{name}` → `{"name", "value"}`;
- `GET /resources/mcp/{name}` → the MCP config with every `${secret:NAME}`
  substituted;
- `GET /resources/skills/{name}` → the skill's `tar.gz` (`application/gzip`).

Rules shared by all three:

- authorization is the same grant check as the manifest; a resource that is
  missing **or** not granted answers 404, so a peer cannot probe names;
- responses that contain secret plaintext (`secrets`, `mcp`) carry
  `Cache-Control: no-store` and write one `secret.fetch` audit row per secret
  revealed, with actor = the peer id;
- an MCP config that references a secret the peer is not granted answers 409
  with the missing names — never a half-substituted config.

The console side lives under `/admin/resources*` and is write-only for secret
values: no admin response ever contains secret plaintext.

## Consequences

- These are the only endpoints that return secret plaintext. Plaintext exists
  in the database only as AES-GCM ciphertext under `data_dir/secrets.key`
  (0600), a key separate from the Muse cookie key.
- Losing `secrets.key` makes every stored secret unrecoverable; the operator
  backs it up together with the database.
- Skill archives are stored and served as uploaded; the relay validates
  structure (`SKILL.md` frontmatter, no path traversal, size cap) but never
  executes them.
