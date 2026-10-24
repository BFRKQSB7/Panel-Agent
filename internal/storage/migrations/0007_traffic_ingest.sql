CREATE TABLE traffic_ledgers (
    machine_id TEXT PRIMARY KEY REFERENCES machines(machine_id),
    ledger_epoch TEXT NOT NULL,
    installation_id TEXT NOT NULL,
    allocated_close_seq INTEGER NOT NULL CHECK (allocated_close_seq BETWEEN 0 AND 9223372036854775807),
    closed_through INTEGER NOT NULL CHECK (closed_through BETWEEN 0 AND 9223372036854775807),
    updated_at TEXT NOT NULL,
    CHECK (closed_through <= allocated_close_seq)
);

CREATE TABLE traffic_segments (
    machine_id TEXT NOT NULL,
    ledger_epoch TEXT NOT NULL,
    stream_id TEXT NOT NULL,
    segment_id TEXT NOT NULL,
    body TEXT NOT NULL,
    accepted_up INTEGER NOT NULL CHECK (accepted_up BETWEEN 0 AND 9223372036854775807),
    accepted_down INTEGER NOT NULL CHECK (accepted_down BETWEEN 0 AND 9223372036854775807),
    finalized INTEGER NOT NULL CHECK (finalized IN (0, 1)),
    close_seq INTEGER CHECK (close_seq IS NULL OR close_seq BETWEEN 1 AND 9223372036854775807),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (machine_id, ledger_epoch, stream_id, segment_id),
    UNIQUE (machine_id, ledger_epoch, close_seq),
    CHECK ((finalized = 0 AND close_seq IS NULL) OR (finalized = 1 AND close_seq IS NOT NULL))
);

CREATE TABLE traffic_closed_segments (
    machine_id TEXT NOT NULL,
    ledger_epoch TEXT NOT NULL,
    stream_id TEXT NOT NULL,
    segment_id TEXT NOT NULL,
    close_seq INTEGER NOT NULL CHECK (close_seq BETWEEN 1 AND 9223372036854775807),
    body TEXT NOT NULL,
    closed_at TEXT NOT NULL,
    PRIMARY KEY (machine_id, ledger_epoch, stream_id, segment_id),
    UNIQUE (machine_id, ledger_epoch, close_seq)
);

CREATE TABLE traffic_usage (
    machine_id TEXT NOT NULL,
    ledger_epoch TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    period_id TEXT NOT NULL,
    uplink INTEGER NOT NULL CHECK (uplink BETWEEN 0 AND 9223372036854775807),
    downlink INTEGER NOT NULL CHECK (downlink BETWEEN 0 AND 9223372036854775807),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (machine_id, ledger_epoch, scope_key, period_id)
);

CREATE TABLE traffic_reports (
    report_id INTEGER PRIMARY KEY,
    machine_id TEXT NOT NULL,
    ledger_epoch TEXT NOT NULL,
    request_id TEXT NOT NULL,
    row_index INTEGER NOT NULL CHECK (row_index BETWEEN 0 AND 255),
    segment_id TEXT NOT NULL,
    body TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('cumulative', 'final', 'retired', 'rejected')),
    error_code TEXT NOT NULL DEFAULT '',
    received_at TEXT NOT NULL
);

CREATE INDEX traffic_segments_close_seq ON traffic_segments(machine_id, ledger_epoch, close_seq);
CREATE INDEX traffic_closed_segments_close_seq ON traffic_closed_segments(machine_id, ledger_epoch, close_seq);
CREATE INDEX traffic_usage_period ON traffic_usage(machine_id, ledger_epoch, period_id);
CREATE INDEX traffic_reports_segment ON traffic_reports(machine_id, ledger_epoch, segment_id, report_id);
