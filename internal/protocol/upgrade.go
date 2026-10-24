package protocol

const (
	UpgradeProtocolVersion = 1

	UpgradeSourceOfficialXray = "official_xray_release"

	UpgradeStatePending   = "pending"
	UpgradeStateRunning   = "running"
	UpgradeStateSucceeded = "succeeded"
	UpgradeStateFailed    = "failed"
	UpgradeStateCanceled  = "canceled"

	UpgradeErrorInvalidIntent = "invalid_upgrade_intent"
	UpgradeErrorDownload      = "upgrade_download_failed"
	UpgradeErrorChecksum      = "upgrade_checksum_mismatch"
	UpgradeErrorLowDisk       = "upgrade_low_disk"
	UpgradeErrorUnsupported   = "upgrade_unsupported"
	UpgradeErrorConfig        = "upgrade_config_invalid"
	UpgradeErrorStart         = "upgrade_start_failed"
	UpgradeErrorHealth        = "upgrade_health_failed"
	UpgradeErrorRollback      = "upgrade_rollback_failed"
	UpgradeErrorCoreDisabled  = "core_not_enabled"
)

const (
	MaxUpgradeVersionBytes = 32
	MaxUpgradePackageBytes = 64 << 20
	MaxUpgradeDetailBytes  = 512
)

type UpgradeIntent struct {
	Version        int    `json:"version"`
	ID             string `json:"id"`
	MachineID      string `json:"machine_id"`
	AuthorityEpoch string `json:"authority_epoch"`
	TargetVersion  string `json:"target_version"`
	Source         string `json:"source"`
	SHA256         string `json:"sha256"`
	PackageSize    uint64 `json:"package_size"`
	State          string `json:"state"`
	ResultCode     string `json:"result_code,omitempty"`
	ResultDetail   string `json:"result_detail,omitempty"`
	ActualVersion  string `json:"actual_version,omitempty"`
	RolledBack     bool   `json:"rolled_back,omitempty"`
	CoreGeneration uint64 `json:"core_generation,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	StartedAt      string `json:"started_at,omitempty"`
	CompletedAt    string `json:"completed_at,omitempty"`
}

type UpgradePullRequest struct {
	Version        int    `json:"version"`
	RequestID      string `json:"request_id"`
	AuthorityEpoch string `json:"authority_epoch"`
}

type UpgradePullResponse struct {
	Version        int             `json:"version"`
	RequestID      string          `json:"request_id"`
	AuthorityEpoch string          `json:"authority_epoch"`
	Intents        []UpgradeIntent `json:"intents"`
}

type UpgradeResultRequest struct {
	Version        int    `json:"version"`
	RequestID      string `json:"request_id"`
	IntentID       string `json:"intent_id"`
	AuthorityEpoch string `json:"authority_epoch"`
	State          string `json:"state"`
	TargetVersion  string `json:"target_version"`
	ActualVersion  string `json:"actual_version,omitempty"`
	ResultCode     string `json:"result_code,omitempty"`
	ResultDetail   string `json:"result_detail,omitempty"`
	RolledBack     bool   `json:"rolled_back,omitempty"`
	CoreGeneration uint64 `json:"core_generation,omitempty"`
}

type UpgradeResultResponse struct {
	Version   int    `json:"version"`
	RequestID string `json:"request_id"`
	Accepted  bool   `json:"accepted"`
}
