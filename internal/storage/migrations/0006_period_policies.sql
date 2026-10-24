CREATE TABLE period_policies (
    policy_id TEXT PRIMARY KEY,
    scope_key TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    effective_at TEXT NOT NULL,
    policy_json TEXT NOT NULL,
    UNIQUE(scope_key,effective_at)
);
CREATE INDEX period_policies_scope ON period_policies(scope_key,effective_at);
