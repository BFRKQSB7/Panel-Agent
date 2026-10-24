CREATE TABLE protocol_extensions (
    extension_id TEXT NOT NULL,
    version TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL,
    package_json BLOB NOT NULL,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (extension_id, version)
);

CREATE TABLE node_extensions (
    node_id TEXT PRIMARY KEY REFERENCES nodes(node_id),
    machine_id TEXT NOT NULL REFERENCES machines(machine_id),
    extension_id TEXT NOT NULL,
    extension_version TEXT NOT NULL,
    public_config TEXT NOT NULL,
    encrypted_secrets BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (extension_id, extension_version) REFERENCES protocol_extensions(extension_id, version)
);
CREATE INDEX node_extensions_machine_idx ON node_extensions(machine_id);
