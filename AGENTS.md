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

- The web-update worker has **one source of truth**: `cmd/agent-relay/update-helper.sh`
  (embedded in the binary, extracted by `install.sh` via `--print-update-helper`, and
  self-refreshed from the new binary after each successful update). Never add a second copy
  to `install.sh`. Its log protocol (`STEP …` / `UPDATE_RESULT …`) is parsed by
  `internal/gateway/update.go`; `TestUpdateHelperProtocolContract` fails on drift.
- Update-job state is disk-backed on purpose (`data_dir/update-jobs/`): the worker restarts
  the gateway mid-job, so anything kept only in memory is lost to the console — that is how
  a successful update ended up displayed as “running” forever. The console JS that follows a
  job has its own test: `make test-ui-js` (`internal/web/ui/_update-panel.test.mjs`, run in
  CI, excluded from the embedded assets by its `_` prefix).
- `relay-watch.sh` exists as **two byte-identical copies**
  (`clients/muse/` + `internal/web/clients/`); edit one, `cp` the other.
  `internal/web` tests compare them.
- `internal/web/views/*_templ.go` **is committed** (a clean checkout must build
  with plain `go build`: no CLI, no network, no build chain). After editing a
  `.templ` file run `make generate` and commit both; `make generate-check`,
  CI, and the release workflow all fail on drift.
- Console assets are served from the embedded `ui/` root, so a template may
  only reference `/style.css`, `/app.js`, `/vendor/...` — **not** `/ui/...`
  (that resolves to `ui/ui/...` and 404s). No external CDNs: vendor libraries
  into `internal/web/ui/vendor/`.
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
