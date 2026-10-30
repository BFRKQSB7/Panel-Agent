CREATE TABLE traffic_retired_ordinals (
    machine_id TEXT NOT NULL,
    ledger_epoch TEXT NOT NULL,
    first INTEGER NOT NULL CHECK(first>0),
    last INTEGER NOT NULL CHECK(last>=first),
    PRIMARY KEY(machine_id,ledger_epoch,first)
);
ALTER TABLE traffic_usage ADD COLUMN period_end TEXT NOT NULL DEFAULT '';
CREATE TABLE traffic_retention (
    id INTEGER PRIMARY KEY CHECK (id=1),
    raw_days INTEGER NOT NULL CHECK (raw_days BETWEEN 1 AND 30),
    raw_rows INTEGER NOT NULL CHECK (raw_rows BETWEEN 1 AND 100000),
    closed_rows INTEGER NOT NULL CHECK (closed_rows BETWEEN 1 AND 10000),
    hour_days INTEGER NOT NULL CHECK (hour_days BETWEEN 1 AND 31),
    day_days INTEGER NOT NULL CHECK (day_days BETWEEN 1 AND 366),
    month_months INTEGER NOT NULL CHECK (month_months BETWEEN 1 AND 120)
);
INSERT INTO traffic_retention VALUES(1,7,10000,1024,7,90,24);
CREATE TABLE traffic_rollups (
    machine_id TEXT NOT NULL,
    ledger_epoch TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    resolution TEXT NOT NULL CHECK (resolution IN ('hour','day','month')),
    bucket TEXT NOT NULL,
    uplink INTEGER NOT NULL CHECK (uplink BETWEEN 0 AND 9223372036854775807),
    downlink INTEGER NOT NULL CHECK (downlink BETWEEN 0 AND 9223372036854775807),
    backfilled INTEGER NOT NULL CHECK (backfilled IN (0,1)),
    estimated INTEGER NOT NULL CHECK (estimated=1),
    PRIMARY KEY(machine_id,ledger_epoch,scope_key,resolution,bucket)
);
CREATE INDEX traffic_reports_retention ON traffic_reports(machine_id,received_at,report_id);
CREATE INDEX traffic_rollups_retention ON traffic_rollups(resolution,bucket);
CREATE INDEX traffic_segments_creation ON traffic_segments(machine_id,ledger_epoch,substr(segment_id,1,36));
CREATE INDEX traffic_closed_creation ON traffic_closed_segments(machine_id,ledger_epoch,substr(segment_id,1,36));
