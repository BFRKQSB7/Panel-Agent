CREATE TABLE traffic_recovery_gaps (
    machine_id TEXT NOT NULL REFERENCES machines(machine_id),
    ledger_epoch TEXT NOT NULL,
    first_seq INTEGER NOT NULL CHECK (first_seq BETWEEN 1 AND 9223372036854775807),
    last_seq INTEGER NOT NULL CHECK (last_seq BETWEEN 1 AND 9223372036854775807 AND last_seq >= first_seq),
    evidence_closed_through INTEGER NOT NULL CHECK (evidence_closed_through BETWEEN 1 AND 9223372036854775807),
    evidence_digest TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (machine_id, ledger_epoch, first_seq, last_seq)
);
CREATE INDEX traffic_recovery_gaps_machine ON traffic_recovery_gaps(machine_id, ledger_epoch, first_seq);
