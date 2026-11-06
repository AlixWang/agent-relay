-- agent-relay schema v1 (DESIGN §5.1 + §6.1 fuse watermarks + §8.4 verify
-- + §6.5 permission handshake + §8.6 prompt distribution + §4.10 work item layer/Phase 2-4).
-- Human-readable reference; the binary executes internal/store/schema.sql,
-- which must be kept identical to this file.

CREATE TABLE IF NOT EXISTS peers (
    id            TEXT PRIMARY KEY,
    display_name  TEXT DEFAULT '',
    agent_type    TEXT DEFAULT 'generic',
    protocol_ver  INTEGER DEFAULT 1,
    capabilities  TEXT DEFAULT '{}',
    status        TEXT DEFAULT 'pending',
    created_at    INTEGER NOT NULL,
    last_seen     INTEGER DEFAULT 0
);
-- Prompt distribution (§8.6).
ALTER TABLE peers ADD COLUMN prompt_version INTEGER DEFAULT 0;
ALTER TABLE peers ADD COLUMN prompt_updated_at INTEGER DEFAULT 0;
-- Assistant profile (§8.7): self-reported capabilities + usual tasks for
-- routing unassigned work. Set at register, refreshable via heartbeat.
ALTER TABLE peers ADD COLUMN profile TEXT DEFAULT '';
ALTER TABLE peers ADD COLUMN profile_updated_at INTEGER DEFAULT 0;
-- Receiver client version (§8.9): which relay-tail binary the peer runs.
-- Reported in heartbeat as client_version (release tag, for display and
-- download) + client_rev (receiver source revision, the update predicate);
-- empty = not reporting (shell scripts have no version). The server nudges
-- only when client_rev differs, same channel as prompt_update. Advisory
-- only, never gates protocol access.
ALTER TABLE peers ADD COLUMN client_version TEXT DEFAULT '';
ALTER TABLE peers ADD COLUMN client_updated_at INTEGER DEFAULT 0;
ALTER TABLE peers ADD COLUMN client_rev TEXT DEFAULT '';
-- Memory reconciliation (§8.6, prompt v11): on prompt_update the assistant
-- purges conflicting relay rules from its own memory and reports a one-line
-- summary via heartbeat (memory_reconciled + memory_version). Display only.
ALTER TABLE peers ADD COLUMN memory_note TEXT DEFAULT '';
ALTER TABLE peers ADD COLUMN memory_version INTEGER DEFAULT 0;
ALTER TABLE peers ADD COLUMN memory_reconciled_at INTEGER DEFAULT 0;
-- Admin remark: operator-only note (e.g. login email) for the members
-- console. Written only via PATCH /admin/peers, served only on
-- GET /admin/peers, never exposed on the agent-facing GET /peers nor
-- sent down to assistants in any prompt.
ALTER TABLE peers ADD COLUMN admin_note TEXT DEFAULT '';
-- Resource hub receipt (§4.12, ADR 0004): the manifest digest the assistant
-- last applied, reported in heartbeat as resources_rev. Empty = not reporting.
ALTER TABLE peers ADD COLUMN resources_rev TEXT DEFAULT '';
ALTER TABLE peers ADD COLUMN resources_updated_at INTEGER DEFAULT 0;
-- Self-service resource registration (ADR 0006): per-peer switch for the
-- submission endpoints. 1 = allowed (subject to the global config switch),
-- 0 = this peer may not submit resources. Admin-only; never exposed to agents.
ALTER TABLE peers ADD COLUMN resource_submit_enabled INTEGER NOT NULL DEFAULT 1;
-- NOTE: tokens.peer_id intentionally has NO foreign key to peers(id).
-- Tokens outlive peers by design (DESIGN §10.2): peers are pruned after
-- peer_prune_after_days, tokens persist until revoked so a returning
-- assistant can still authenticate and re-register presence.
CREATE TABLE IF NOT EXISTS tokens (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    peer_id       TEXT NOT NULL,
    token_hash    TEXT NOT NULL UNIQUE,
    label         TEXT DEFAULT '',
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER DEFAULT 0,
    last_ip       TEXT DEFAULT '',
    revoked_at    INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS invites (
    code_hash     TEXT PRIMARY KEY,
    intended_id   TEXT DEFAULT '',
    agent_type    TEXT DEFAULT 'generic',
    created_by    TEXT DEFAULT 'admin',
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    used_by       TEXT DEFAULT '',
    used_at       INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    id            TEXT NOT NULL,
    sender        TEXT NOT NULL,
    recipient     TEXT NOT NULL,
    kind          TEXT DEFAULT 'task',
    in_reply_to   TEXT DEFAULT '',
    root_id       TEXT DEFAULT '',
    requires_approval INTEGER DEFAULT 0,
    approval_state TEXT DEFAULT 'n/a',
    payload       TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    UNIQUE(sender, id)
);
CREATE TABLE IF NOT EXISTS acks (
    message_seq   INTEGER NOT NULL REFERENCES messages(seq),
    peer_id       TEXT NOT NULL,
    acked_at      INTEGER NOT NULL,
    PRIMARY KEY (message_seq, peer_id)
);
-- Per-(peer, scope) contiguous read watermark. This is the single source of
-- truth for delivery position and read counts: a peer has read every message
-- in the scope whose seq is <= watermark. `acks` stays as an audit trail and
-- idempotency record, never as the delivery frontier. See DESIGN §5.4.
CREATE TABLE IF NOT EXISTS peer_read_watermarks (
    scope      TEXT NOT NULL,   -- thread:<root> | room:<grp> | dm:<a>|<b>
    peer_id    TEXT NOT NULL,
    seq        INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (scope, peer_id)
);
CREATE INDEX IF NOT EXISTS idx_read_watermarks_peer ON peer_read_watermarks(peer_id);
-- Work-item projection (docs/adr/0001-work-items-as-projection.md). Tickets
-- are derived from direct kind=task messages; messages remain the only write
-- model. message_tickets maps every follow-up message to its nearest task
-- ancestor, so a thread may carry several independent tickets.
CREATE TABLE IF NOT EXISTS tickets (
    id             INTEGER PRIMARY KEY,
    thread_root    TEXT NOT NULL DEFAULT '',
    requester      TEXT NOT NULL,
    assignee       TEXT NOT NULL,
    state          TEXT NOT NULL DEFAULT 'open',
    stage          TEXT NOT NULL DEFAULT 'queued',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    ended_at       INTEGER NOT NULL DEFAULT 0,
    last_seq       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS policy_versions (
    version    TEXT PRIMARY KEY,
    rules      TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS mandates (
    ticket_id      INTEGER PRIMARY KEY REFERENCES tickets(id) ON DELETE CASCADE,
    policy_version TEXT NOT NULL DEFAULT '',
    scope          TEXT NOT NULL DEFAULT 'task',
    requester      TEXT NOT NULL,
    requester_kind TEXT NOT NULL DEFAULT 'peer',
    created_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS message_tickets (
    message_seq INTEGER PRIMARY KEY REFERENCES messages(seq) ON DELETE CASCADE,
    ticket_id   INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_tickets_stage ON tickets(stage, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_tickets_assignee ON tickets(assignee, state, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_message_tickets_ticket ON message_tickets(ticket_id, message_seq);
CREATE TABLE IF NOT EXISTS ticket_assignments (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    ticket_id        INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    assignee         TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending', -- pending | running | ended
    lease_expires_at INTEGER NOT NULL DEFAULT 0,
    ended_reason     TEXT NOT NULL DEFAULT '',        -- reassigned | lease_expired | ticket_closed
    created_at       INTEGER NOT NULL,
    started_at       INTEGER NOT NULL DEFAULT 0,
    ended_at         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ticket_assignments_ticket ON ticket_assignments(ticket_id, status);
CREATE TABLE IF NOT EXISTS ticket_subscriptions (
    ticket_id   INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    thread_root TEXT NOT NULL,
    mode        TEXT NOT NULL DEFAULT 'own', -- own | context
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (ticket_id, thread_root)
);
CREATE INDEX IF NOT EXISTS idx_ticket_subscriptions_thread ON ticket_subscriptions(thread_root, ticket_id);
-- Durable side-effect queue (docs/SUPERAGENT.md Phase 3). A message or
-- presence transition enqueues intent inside its transaction; the effects
-- worker executes it after commit with backoff.
CREATE TABLE IF NOT EXISTS effects (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,
    key        TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '',
    attempts   INTEGER NOT NULL DEFAULT 0,
    next_at    INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    UNIQUE(kind, key)
);
CREATE INDEX IF NOT EXISTS idx_effects_due ON effects(next_at);
-- Immutable per-template prompt revisions (docs/SUPERAGENT.md Phase 4).
-- A template change produces a new hash row; old rows are never edited.
CREATE TABLE IF NOT EXISTS prompt_revisions (
    revision      TEXT PRIMARY KEY,
    agent_type    TEXT NOT NULL,
    base_revision TEXT NOT NULL DEFAULT '',
    content       TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_log (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            INTEGER NOT NULL,
    actor         TEXT NOT NULL,
    action        TEXT NOT NULL,
    detail        TEXT DEFAULT ''
);
-- Agent runtime diagnostics (DESIGN §4.11): structured logs uploaded by
-- clients. Deliberately separate from audit_log: these are high-volume,
-- short-lived operational records, not security/action audit history.
CREATE TABLE IF NOT EXISTS agent_logs (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id       TEXT NOT NULL,
    ts             INTEGER NOT NULL,
    received_at    INTEGER NOT NULL,
    peer_id        TEXT NOT NULL,
    level          TEXT NOT NULL,
    source         TEXT NOT NULL,
    event          TEXT NOT NULL,
    message        TEXT NOT NULL DEFAULT '',
    task_id        TEXT NOT NULL DEFAULT '',
    thread         TEXT NOT NULL DEFAULT '',
    fields         TEXT NOT NULL DEFAULT '{}',
    client_version TEXT NOT NULL DEFAULT '',
    client_rev     TEXT NOT NULL DEFAULT '',
    UNIQUE(peer_id, event_id)
);
CREATE TABLE IF NOT EXISTS thread_fuses (
    root_id       TEXT PRIMARY KEY,
    cleared_seq   INTEGER DEFAULT 0,
    updated_at    INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS verify_state (
    peer_id       TEXT PRIMARY KEY,
    smoke_id      TEXT DEFAULT '',
    updated_at    INTEGER DEFAULT 0
);
-- Execution-state columns for remote approval (§6.5).
ALTER TABLE messages ADD COLUMN status TEXT DEFAULT '';
ALTER TABLE messages ADD COLUMN op TEXT DEFAULT '';
ALTER TABLE messages ADD COLUMN target TEXT DEFAULT '';
ALTER TABLE messages ADD COLUMN detail TEXT DEFAULT '';
ALTER TABLE messages ADD COLUMN decision TEXT DEFAULT '';
ALTER TABLE messages ADD COLUMN expires_at INTEGER DEFAULT 0;
ALTER TABLE messages ADD COLUMN scope TEXT DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_messages_scope_seq ON messages(scope, seq);
CREATE TABLE IF NOT EXISTS permission_requests (
    request_id    TEXT PRIMARY KEY,
    thread        TEXT NOT NULL,
    requester     TEXT NOT NULL,
    approver      TEXT NOT NULL,
    status        TEXT DEFAULT 'pending',
    op            TEXT DEFAULT '',
    target        TEXT DEFAULT '',
    detail        TEXT DEFAULT '',
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    decided_at    INTEGER DEFAULT 0,
    decision_id   TEXT DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_perm_thread ON permission_requests(thread, status);
-- Rooms (§6.8): a room is a routing alias with a member list, not a peer: it
-- never authenticates and gets no token. One room message is stored once
-- (recipient = room id); membership decides delivery.
CREATE TABLE IF NOT EXISTS rooms (
    id            TEXT PRIMARY KEY,
    name          TEXT DEFAULT '',
    note          TEXT DEFAULT '',
    created_at    INTEGER NOT NULL,
    archived_at   INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS room_members (
    room_id       TEXT NOT NULL,
    peer_id       TEXT NOT NULL,
    added_at      INTEGER NOT NULL,
    start_seq     INTEGER DEFAULT 0,
    PRIMARY KEY (room_id, peer_id)
);
-- Console key/value state (§9.5): currently the operator inbox read watermark.
CREATE TABLE IF NOT EXISTS settings (
    key           TEXT PRIMARY KEY,
    value         TEXT DEFAULT '',
    updated_at    INTEGER DEFAULT 0
);
-- Muse account authorization + keepalive (§4.9): one Muse account per relay
-- peer. cookies_enc is AES-GCM ciphertext (key in data_dir/muse-cookie.key,
-- 0600) and is never returned by the admin API. No FK on peer_id: DeletePeer
-- and PrunePeers clean the binding in their own transactions.
CREATE TABLE IF NOT EXISTS muse_accounts (
    id                TEXT PRIMARY KEY,
    peer_id           TEXT NOT NULL UNIQUE,
    label             TEXT DEFAULT '',
    cookies_enc       TEXT DEFAULT '',
    cookies_exp       TEXT DEFAULT '{}',
    session_status    TEXT DEFAULT '',
    vm_id             TEXT DEFAULT '',
    vm_state          TEXT DEFAULT '',
    auth_state        TEXT DEFAULT 'unverified',
    last_error        TEXT DEFAULT '',
    last_keepalive_at INTEGER DEFAULT 0,
    last_attempt_at   INTEGER DEFAULT 0,
    last_attempt_ok   INTEGER DEFAULT 0,
    last_wake_at      INTEGER DEFAULT 0,
    last_wake_ok      INTEGER DEFAULT 0,
    auto_wake         INTEGER DEFAULT 1,
    enabled           INTEGER DEFAULT 1,
    authorized_at     INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
-- Resource hub (§4.12, ADR 0004/0005): register once, grant per peer, distribute.
-- resource_revisions is append-only history; resources points each (kind,name)
-- at its live revision. content is AES-GCM ciphertext for kind='secret' (key in
-- data_dir/secrets.key, 0600, never returned by the admin API); content_hash is
-- an HMAC for secrets and a SHA-256 otherwise. resource_grants.peer_id='*'
-- grants every peer. No FK on peer_id: DeletePeer/PrunePeers clean grants in
-- their own transactions, same as muse_accounts.
CREATE TABLE IF NOT EXISTS resource_revisions (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    content       BLOB NOT NULL,
    content_hash  TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    created_by    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS resources (
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    revision_id   TEXT NOT NULL,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (kind, name)
);
-- Adds owner_peer for both existing and freshly created databases (duplicate
-- column is ignored by the boot migration helper).
ALTER TABLE resources ADD COLUMN owner_peer TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS resource_grants (
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    peer_id       TEXT NOT NULL,
    granted_at    INTEGER NOT NULL,
    PRIMARY KEY (kind, name, peer_id)
);
-- Self-service resource registration (ADR 0006). Submissions are staged here
-- until an operator approves them; pending rows never appear in a manifest.
-- content is AES-GCM ciphertext for kind='secret'. Rejected/withdrawn/expired
-- rows keep only metadata and a hash: the content blob is wiped.
CREATE TABLE IF NOT EXISTS resource_submissions (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    content       BLOB NOT NULL,
    content_hash  TEXT NOT NULL,
    submitted_by  TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    review_note   TEXT NOT NULL DEFAULT '',
    reviewed_by   TEXT NOT NULL DEFAULT '',
    reviewed_at   INTEGER NOT NULL DEFAULT 0,
    approved_name TEXT NOT NULL DEFAULT '',
    revision_id   TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS resource_takedowns (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    requested_by  TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending',
    review_note   TEXT NOT NULL DEFAULT '',
    reviewed_by   TEXT NOT NULL DEFAULT '',
    reviewed_at   INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_resource_submissions_pending
    ON resource_submissions(submitted_by, kind, name) WHERE status='pending';
CREATE INDEX IF NOT EXISTS idx_resource_submissions_peer
    ON resource_submissions(submitted_by, status, created_at);
CREATE INDEX IF NOT EXISTS idx_resource_submissions_status
    ON resource_submissions(status, kind, created_at);
CREATE INDEX IF NOT EXISTS idx_resource_takedowns_status
    ON resource_takedowns(status, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_resource_takedowns_pending
    ON resource_takedowns(kind, name) WHERE status='pending';
CREATE INDEX IF NOT EXISTS idx_room_members_peer ON room_members(peer_id, room_id);
CREATE INDEX IF NOT EXISTS idx_messages_recipient_seq ON messages(recipient, seq);
CREATE INDEX IF NOT EXISTS idx_messages_root ON messages(root_id, seq);
CREATE INDEX IF NOT EXISTS idx_messages_sender_id ON messages(sender, id);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts);
CREATE INDEX IF NOT EXISTS idx_agent_logs_received ON agent_logs(received_at);
CREATE INDEX IF NOT EXISTS idx_agent_logs_peer_received ON agent_logs(peer_id, received_at);
CREATE INDEX IF NOT EXISTS idx_agent_logs_level_received ON agent_logs(level, received_at);
CREATE INDEX IF NOT EXISTS idx_agent_logs_event_received ON agent_logs(event, received_at);
CREATE INDEX IF NOT EXISTS idx_resource_revisions_kind_name ON resource_revisions(kind, name, created_at);
CREATE INDEX IF NOT EXISTS idx_resource_grants_peer ON resource_grants(peer_id);
