package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"panel-agent.local/panel-agent/internal/period"
)

const AgentVersion = "0.1.0"

const (
	ErrorInvalidJoinToken        = "invalid_join_token"
	ErrorExpiredJoinToken        = "expired_join_token"
	ErrorUsedJoinToken           = "used_join_token"
	ErrorRegistrationConflict    = "registration_conflict"
	ErrorAuthenticationRequired  = "authentication_required"
	ErrorMachineRevoked          = "machine_revoked"
	ErrorMachineNotFound         = "machine_not_found"
	ErrorAuthorityMismatch       = "authority_mismatch"
	ErrorStaleRevision           = "stale_revision"
	ErrorDigestMismatch          = "digest_mismatch"
	ErrorInvalidMessage          = "invalid_message"
	ErrorCoreNotImplemented      = "core_not_implemented"
	ErrorDesiredStateNotReceived = "desired_state_not_received"
	ErrorTrafficState            = "traffic_state_conflict"
	ErrorTrafficRecoveryGap      = "traffic_recovery_gap"
	ErrorTrafficReceipt          = "traffic_receipt_invalid"
)

type ErrorResponse struct {
	Error ErrorDetails `json:"error"`
}

type ErrorDetails struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const CoreStatsPort = 10085

type DesiredNode struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	PublicHost  string            `json:"public_host"`
	PublicPort  int               `json:"public_port"`
	ListenHost  string            `json:"listen_host"`
	ListenPort  int               `json:"listen_port"`
	Enabled     bool              `json:"enabled"`
	Reality     RealityConfig     `json:"reality"`
	Credentials []ProxyCredential `json:"credentials"`
}

type RealityConfig struct {
	ServerName string `json:"server_name"`
	Target     string `json:"target"`
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
	ShortID    string `json:"short_id"`
}

type ProxyCredential struct {
	UserID      string `json:"user_id"`
	UUID        string `json:"uuid"`
	Email       string `json:"email"`
	Development bool   `json:"development"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

type DesiredState struct {
	Nodes                   []DesiredNode     `json:"nodes"`
	PeriodPolicies          []period.Policy   `json:"period_policies,omitempty"`
	TrafficReportingEnabled bool              `json:"traffic_reporting_enabled,omitempty"`
	QuotaRestrictions       QuotaRestrictions `json:"quota_restrictions,omitempty"`
}

type RegisterRequest struct {
	RegistrationID  string `json:"registration_id"`
	JoinToken       string `json:"join_token"`
	CandidateSecret string `json:"candidate_secret"`
	TargetMachineID string `json:"target_machine_id,omitempty"`
	Hostname        string `json:"hostname"`
	DisplayName     string `json:"display_name,omitempty"`
	OS              string `json:"os"`
	Architecture    string `json:"architecture"`
	CPUCount        int    `json:"cpu_count"`
	MemoryBytes     uint64 `json:"memory_bytes"`
	AgentVersion    string `json:"agent_version"`
}

type RegisterResponse struct {
	MachineID      string        `json:"machine_id"`
	RegistrationID string        `json:"registration_id"`
	AuthorityEpoch string        `json:"authority_epoch"`
	Revision       uint64        `json:"revision"`
	Digest         string        `json:"digest"`
	DesiredState   *DesiredState `json:"desired_state,omitempty"`
}

type RecoveryRequest struct {
	RegistrationID string `json:"registration_id"`
}

type DesiredStateRequest struct {
	RequestID            string `json:"request_id"`
	AuthorityEpoch       string `json:"authority_epoch"`
	Revision             uint64 `json:"revision"`
	Digest               string `json:"digest"`
	AuthorityNonce       string `json:"authority_nonce,omitempty"`
	FullSnapshotReceived bool   `json:"full_snapshot_received,omitempty"`
}

type DesiredStateResponse struct {
	RequestID         string        `json:"request_id"`
	AuthorityEpoch    string        `json:"authority_epoch"`
	Revision          uint64        `json:"revision"`
	Digest            string        `json:"digest"`
	Unchanged         bool          `json:"unchanged"`
	DesiredState      *DesiredState `json:"desired_state,omitempty"`
	AuthorityChange   bool          `json:"authority_change,omitempty"`
	AuthorityNonce    string        `json:"authority_nonce,omitempty"`
	AcknowledgedNonce string        `json:"acknowledged_nonce,omitempty"`
}

type StatusRequest struct {
	RequestID                string `json:"request_id"`
	AuthorityEpoch           string `json:"authority_epoch"`
	Revision                 uint64 `json:"revision"`
	Digest                   string `json:"digest"`
	Hostname                 string `json:"hostname"`
	DisplayName              string `json:"display_name,omitempty"`
	OS                       string `json:"os"`
	Architecture             string `json:"architecture"`
	CPUCount                 int    `json:"cpu_count"`
	MemoryBytes              uint64 `json:"memory_bytes"`
	AgentVersion             string `json:"agent_version"`
	Received                 bool   `json:"received"`
	Applied                  bool   `json:"applied"`
	AppliedRevision          uint64 `json:"applied_revision,omitempty"`
	AppliedDigest            string `json:"applied_digest,omitempty"`
	ApplyError               string `json:"apply_error,omitempty"`
	AppliedAuthorityEpoch    string `json:"applied_authority_epoch,omitempty"`
	CoreGeneration           uint64 `json:"core_generation,omitempty"`
	CoreInstanceID           string `json:"core_instance_id,omitempty"`
	AppliedRestrictionDigest string `json:"applied_restriction_digest,omitempty"`
	RestrictionAppliedAt     string `json:"restriction_applied_at,omitempty"`
}

type StatusResponse struct {
	Accepted      bool   `json:"accepted"`
	LastHeartbeat string `json:"last_heartbeat"`
}

type JoinTokenResponse struct {
	JoinToken string `json:"join_token"`
	ExpiresAt string `json:"expires_at"`
}

type MachineView struct {
	MachineID                string `json:"machine_id"`
	Hostname                 string `json:"hostname"`
	DisplayName              string `json:"display_name"`
	Notes                    string `json:"notes"`
	OS                       string `json:"os"`
	Architecture             string `json:"architecture"`
	CPUCount                 int    `json:"cpu_count"`
	MemoryBytes              uint64 `json:"memory_bytes"`
	AgentVersion             string `json:"agent_version"`
	AuthorityEpoch           string `json:"authority_epoch"`
	DesiredRevision          uint64 `json:"desired_revision"`
	ReceivedRevision         uint64 `json:"received_revision"`
	ReceivedDigest           string `json:"received_digest"`
	AppliedRevision          uint64 `json:"applied_revision"`
	AppliedDigest            string `json:"applied_digest"`
	ApplyError               string `json:"apply_error"`
	AppliedAuthorityEpoch    string `json:"applied_authority_epoch"`
	CoreGeneration           uint64 `json:"core_generation"`
	CoreInstanceID           string `json:"core_instance_id"`
	AppliedRestrictionDigest string `json:"applied_restriction_digest,omitempty"`
	RestrictionAppliedAt     string `json:"restriction_applied_at,omitempty"`
	LastHeartbeat            string `json:"last_heartbeat,omitempty"`
	Revoked                  bool   `json:"revoked"`
}

func EmptyDesiredState() DesiredState {
	return DesiredState{Nodes: []DesiredNode{}}
}

func DigestDesiredState(state DesiredState) (string, error) {
	canonical, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
