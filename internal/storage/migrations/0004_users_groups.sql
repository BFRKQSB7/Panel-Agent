CREATE TABLE user_groups (
    group_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    name_key TEXT NOT NULL,
    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX user_groups_live_name ON user_groups(name_key) WHERE deleted_at IS NULL;

CREATE TABLE node_groups (
    group_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    name_key TEXT NOT NULL,
    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX node_groups_live_name ON node_groups(name_key) WHERE deleted_at IS NULL;

CREATE TABLE user_group_node_groups (
    user_group_id TEXT NOT NULL REFERENCES user_groups(group_id),
    node_group_id TEXT NOT NULL REFERENCES node_groups(group_id),
    PRIMARY KEY (user_group_id, node_group_id)
);
CREATE TABLE node_group_nodes (
    node_group_id TEXT NOT NULL REFERENCES node_groups(group_id),
    node_id TEXT NOT NULL REFERENCES nodes(node_id),
    PRIMARY KEY (node_group_id, node_id)
);
CREATE INDEX node_group_nodes_node ON node_group_nodes(node_id);

CREATE TABLE proxy_users (
    user_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    name_key TEXT NOT NULL,
    user_group_id TEXT NOT NULL REFERENCES user_groups(group_id),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    expires_at TEXT,
    quota_bytes INTEGER CHECK (quota_bytes >= 0),
    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX proxy_users_live_name ON proxy_users(name_key) WHERE deleted_at IS NULL;
CREATE INDEX proxy_users_group ON proxy_users(user_group_id);

CREATE TABLE proxy_credentials (
    user_id TEXT NOT NULL REFERENCES proxy_users(user_id),
    protocol TEXT NOT NULL CHECK (protocol = 'vless'),
    encrypted_secret BLOB NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (user_id, protocol)
);
