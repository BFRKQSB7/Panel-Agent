ALTER TABLE nodes ADD COLUMN quota_bytes INTEGER CHECK (quota_bytes >= 0);
ALTER TABLE machines ADD COLUMN applied_restriction_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE machines ADD COLUMN restriction_applied_at TEXT;
ALTER TABLE traffic_ledgers ADD COLUMN reconciliation_pending INTEGER NOT NULL DEFAULT 0 CHECK (reconciliation_pending IN (0, 1));

CREATE TABLE user_node_quotas (
    user_id TEXT NOT NULL REFERENCES proxy_users(user_id),
    node_id TEXT NOT NULL REFERENCES nodes(node_id),
    quota_bytes INTEGER CHECK (quota_bytes >= 0),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (user_id, node_id)
);
CREATE INDEX user_node_quotas_node ON user_node_quotas(node_id);

CREATE TABLE machine_budgets (
    machine_id TEXT PRIMARY KEY REFERENCES machines(machine_id),
    budget_bytes INTEGER CHECK (budget_bytes >= 0),
    warning_bytes INTEGER CHECK (warning_bytes >= 0),
    hard_stop INTEGER NOT NULL CHECK (hard_stop IN (0, 1)),
    updated_at TEXT NOT NULL
);
