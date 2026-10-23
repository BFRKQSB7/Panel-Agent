CREATE TABLE panel_authority (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    authority_epoch TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    digest TEXT NOT NULL,
    snapshot_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE machines (
    machine_id TEXT PRIMARY KEY,
    registration_id TEXT NOT NULL UNIQUE,
    token_hash TEXT NOT NULL UNIQUE,
    hostname TEXT NOT NULL,
    display_name TEXT NOT NULL,
    notes TEXT NOT NULL DEFAULT '',
    os TEXT NOT NULL,
    architecture TEXT NOT NULL,
    cpu_count INTEGER NOT NULL DEFAULT 0,
    memory_bytes INTEGER NOT NULL DEFAULT 0,
    agent_version TEXT NOT NULL,
    authority_epoch TEXT NOT NULL,
    desired_revision INTEGER NOT NULL CHECK (desired_revision > 0),
    desired_digest TEXT NOT NULL,
    received_revision INTEGER NOT NULL DEFAULT 0,
    received_digest TEXT NOT NULL DEFAULT '',
    applied_revision INTEGER NOT NULL DEFAULT 0,
    applied_digest TEXT NOT NULL DEFAULT '',
    apply_error TEXT NOT NULL DEFAULT 'core_not_implemented',
    pending_authority_epoch TEXT,
    pending_authority_nonce TEXT,
    last_authority_nonce TEXT,
    last_heartbeat_at TEXT,
    revoked_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE join_tokens (
    token_hash TEXT PRIMARY KEY,
    target_machine_id TEXT REFERENCES machines(machine_id),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    consumed_at TEXT
);

CREATE INDEX machines_last_heartbeat_idx ON machines(last_heartbeat_at);
CREATE INDEX join_tokens_expiry_idx ON join_tokens(expires_at);
