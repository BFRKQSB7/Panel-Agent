CREATE TABLE admin_user (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE installation (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    installation_id TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TRIGGER installation_immutable_update
BEFORE UPDATE ON installation
BEGIN
    SELECT RAISE(ABORT, 'installation identity is immutable');
END;

CREATE TRIGGER installation_immutable_delete
BEFORE DELETE ON installation
BEGIN
    SELECT RAISE(ABORT, 'installation identity is immutable');
END;
