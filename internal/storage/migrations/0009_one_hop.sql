ALTER TABLE nodes ADD COLUMN exit_machine_id TEXT REFERENCES machines(machine_id);

CREATE TABLE exits (
    machine_id TEXT PRIMARY KEY REFERENCES machines(machine_id),
    public_host TEXT NOT NULL,
    public_port INTEGER NOT NULL CHECK (public_port BETWEEN 1 AND 65535),
    listen_host TEXT NOT NULL,
    listen_port INTEGER NOT NULL CHECK (listen_port BETWEEN 1 AND 65535 AND listen_port != 10085),
    server_name TEXT NOT NULL,
    reality_target TEXT NOT NULL,
    private_key BLOB NOT NULL,
    public_key TEXT NOT NULL,
    short_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE transit_credentials (
    entry_machine_id TEXT NOT NULL REFERENCES machines(machine_id),
    exit_machine_id TEXT NOT NULL REFERENCES machines(machine_id),
    encrypted_uuid BLOB NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (entry_machine_id, exit_machine_id),
    CHECK (entry_machine_id != exit_machine_id)
);
CREATE INDEX transit_credentials_exit ON transit_credentials(exit_machine_id);
