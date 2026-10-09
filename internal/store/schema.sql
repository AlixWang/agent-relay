-- Canonical schema for agent-relay (DESIGN §5.1 + §6.1 fuse watermarks + §8.4 verify).
-- Embedded by internal/store via schema.sql. migrations/001_init.sql is kept
-- as the human-readable reference; this file is what the binary executes.

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
-- Prompt distribution (§8.6): prompt_version tracks which worker-instruction
-- revision each peer runs. Bumped server-side on template change; peers
-- report theirs in heartbeat and pull the delta via /prompts/current.
-- ALTERs so pre-existing databases migrate on boot like the §6.5 columns.
ALTER TABLE peers ADD COLUMN prompt_version INTEGER DEFAULT 0;
ALTER TABLE peers ADD COLUMN prompt_updated_at INTEGER DEFAULT 0;
-- Assistant profile (§8.7): self-reported capabilities + usual tasks for
-- routing unassigned work. Set at register, refreshable via heartbeat.
-- profile_refresh_days (config) controls the server's refresh nudge.
ALTER TABLE peers ADD COLUMN profile TEXT DEFAULT '';
ALTER TABLE peers ADD COLUMN profile_updated_at INTEGER DEFAULT 0;
-- Receiver client version (§8.9): which relay-tail binary the peer runs.
-- client_version is the release tag (for display/download); client_rev is the
-- receiver source revision baked at build time (the actual update predicate:
-- doc/prompt-only releases leave it unchanged, so nobody gets nagged).
ALTER TABLE peers ADD COLUMN client_version TEXT DEFAULT '';
ALTER TABLE peers ADD COLUMN client_updated_at INTEGER DEFAULT 0;
ALTER TABLE peers ADD COLUMN client_rev TEXT DEFAULT '';
-- Memory reconciliation (§8.6, prompt v11): on prompt_update the assistant
-- purges conflicting relay rules from its own memory and reports a one-line
-- summary via heartbeat (memory_reconciled + memory_version). Display only.
ALTER TABLE peers ADD COLUMN memory_note TEXT DEFAULT '';
ALTER TABLE peers ADD COLUMN memory_version INTEGER DEFAULT 0;
ALTER TABLE peers ADD COLUMN memory_reconciled_at INTEGER DEFAULT 0;
-- Reported in heartbeat as client_version; empty means "not reporting"
-- (shell scripts have no version concept). The server nudges via heartbeat
-- when a newer Release exists, same channel as prompt_update. Advisory
-- only: never gates protocol access.
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
CREATE TABLE IF NOT EXISTS audit_log (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            INTEGER NOT NULL,
    actor         TEXT NOT NULL,
    action        TEXT NOT NULL,
    detail        TEXT DEFAULT ''
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
-- Execution-state columns for remote approval (§6.5): status handshake
-- values started progress blocked resumed cancelled, structured permission
-- fields op target detail, decision allow deny, request expiry.
-- ALTERs (not inline columns) so pre-existing databases migrate on boot;
-- migrate() tolerates "duplicate column" on repeat starts.
ALTER TABLE messages ADD COLUMN status TEXT DEFAULT '';
ALTER TABLE messages ADD COLUMN op TEXT DEFAULT '';
ALTER TABLE messages ADD COLUMN target TEXT DEFAULT '';
ALTER TABLE messages ADD COLUMN detail TEXT DEFAULT '';
ALTER TABLE messages ADD COLUMN decision TEXT DEFAULT '';
ALTER TABLE messages ADD COLUMN expires_at INTEGER DEFAULT 0;
-- Permission handshake rows (§6.5). One row per permission_request message
-- where request_id equals the message id. The decision CAS runs on this table.
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
CREATE INDEX IF NOT EXISTS idx_room_members_peer ON room_members(peer_id, room_id);
CREATE INDEX IF NOT EXISTS idx_messages_recipient_seq ON messages(recipient, seq);
CREATE INDEX IF NOT EXISTS idx_messages_root ON messages(root_id, seq);
CREATE INDEX IF NOT EXISTS idx_messages_sender_id ON messages(sender, id);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts);
