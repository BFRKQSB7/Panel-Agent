CREATE TABLE user_tokens (
    user_id TEXT NOT NULL REFERENCES proxy_users(user_id),
    kind TEXT NOT NULL CHECK (kind IN ('dashboard', 'subscription')),
    token_hash TEXT PRIMARY KEY,
    generation INTEGER NOT NULL CHECK (generation > 0),
    updated_at TEXT NOT NULL,
    revoked_at TEXT
);
CREATE UNIQUE INDEX user_tokens_active ON user_tokens(user_id, kind) WHERE revoked_at IS NULL;
CREATE INDEX user_tokens_history ON user_tokens(user_id, kind, generation);

CREATE TABLE token_operations (
    operation_id TEXT NOT NULL,
    user_id TEXT NOT NULL REFERENCES proxy_users(user_id),
    kind TEXT NOT NULL CHECK (kind IN ('dashboard', 'subscription')),
    token_hash TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    created_at TEXT NOT NULL,
    PRIMARY KEY (operation_id, user_id, kind)
);
CREATE INDEX token_operations_created ON token_operations(created_at);
