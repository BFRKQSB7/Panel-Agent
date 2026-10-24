CREATE TABLE upgrade_intents (
    id TEXT PRIMARY KEY,
    machine_id TEXT NOT NULL REFERENCES machines(machine_id),
    authority_epoch TEXT NOT NULL,
    target_version TEXT NOT NULL,
    source TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    package_size INTEGER NOT NULL CHECK (package_size > 0 AND package_size <= 67108864),
    state TEXT NOT NULL CHECK (state IN ('pending','running','succeeded','failed','canceled')),
    result_code TEXT NOT NULL DEFAULT '',
    result_detail TEXT NOT NULL DEFAULT '',
    actual_version TEXT NOT NULL DEFAULT '',
    rolled_back INTEGER NOT NULL DEFAULT 0 CHECK (rolled_back IN (0,1)),
    core_generation INTEGER NOT NULL DEFAULT 0 CHECK (core_generation >= 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    started_at TEXT,
    completed_at TEXT
);
CREATE INDEX upgrade_intents_machine_state ON upgrade_intents(machine_id, state, created_at);
CREATE INDEX upgrade_intents_authority ON upgrade_intents(machine_id, authority_epoch, state);
