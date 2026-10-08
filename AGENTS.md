# AGENTS.md — working rules for this repo

## Release / deploy (hard rules)

1. **Ask before tagging.** Never create a `v*` tag on your own initiative.
   Before tagging, present: proposed version (follow SemVer on the existing
   line), a one-line summary per change, and the prompt/protocol impact.
   Tag only after the user confirms the version number.
2. **Never deploy to production by hand.** Do not `ssh` to the production box
   to swap the binary, restart the service, or run `install.sh`. After the
   Release workflow goes green, hand off: report the tag, the receiver_rev,
   and the asset list, then let the user press *update* in the console
   (`/admin/update`, §10.4). Read-only inspection from the outside is fine.
3. Receiver-side upgrades are not a hand-off either: assistants pull
   `relay-tail` through their own §7.6 flow, woken by `client_update`.

## Change checklists (things that silently break when forgotten)

- `relay-watch.sh` exists as **two byte-identical copies**
  (`clients/muse/` + `internal/web/clients/`); edit one, `cp` the other.
  `internal/web` tests compare them.
- Schema changes go in **both** `internal/store/schema.sql` and
  `migrations/001_init.sql`.
- Prompt text changes are versioned: bump `prompts.PromptVersion`, append a
  `ChangeLog` entry (append-only; small bumps read `changes` before the full
  text), and keep the distribution test
  (`TestDistributionMatchesInit`) passing — the served prompt and the
  first-run prompt must match word for word.
- `client_update` is keyed to `receiver_rev` (hash of `cmd/relay-tail/**/*.go`),
  never to the release tag. If you touch receiver sources, expect a legitimate
  fleet-wide nudge; if you only touch prompt/docs, expect none.
- Update checksum files are matched by name: the download must be saved as the
  exact filename listed in `SHA256SUMS` (`agent-relay-linux-amd64`).

## Local tooling notes

- macOS: no `timeout(1)`, no `cat -A`; BSD `wc -l` pads output (`       3`) —
  strip with `tr -d '[:space:]'` before any numeric guard.
- `go run` cannot import `internal/` from outside the module: print rendered
  output from a temporary test file inside the repo instead.
- Bulk edits inside large Chinese templates: the `edit` tool often fails to
  match; use a `python3` heredoc with an `assert old in s` guard.
