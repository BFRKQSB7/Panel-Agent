ALTER TABLE machines ADD COLUMN desired_snapshot BLOB;
ALTER TABLE machines ADD COLUMN applied_authority_epoch TEXT NOT NULL DEFAULT '';
ALTER TABLE machines ADD COLUMN core_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE machines ADD COLUMN core_instance_id TEXT NOT NULL DEFAULT '';

CREATE TABLE nodes (
    node_id TEXT PRIMARY KEY,
    machine_id TEXT NOT NULL REFERENCES machines(machine_id),
    name TEXT NOT NULL,
    public_host TEXT NOT NULL,
    public_port INTEGER NOT NULL CHECK (public_port BETWEEN 1 AND 65535),
    listen_host TEXT NOT NULL,
    listen_port INTEGER NOT NULL CHECK (listen_port BETWEEN 1 AND 65535 AND listen_port != 10085),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    server_name TEXT NOT NULL,
    reality_target TEXT NOT NULL,
    private_key BLOB NOT NULL,
    public_key TEXT NOT NULL,
    short_id TEXT NOT NULL,
    development INTEGER NOT NULL DEFAULT 0 CHECK (development IN (0, 1)),
    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX nodes_machine_listen_port_idx ON nodes(machine_id, listen_port) WHERE deleted_at IS NULL;

CREATE TABLE development_credential (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    encrypted_uuid BLOB NOT NULL
);
