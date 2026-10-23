package protocol

const (
	ErrorCorePending        = "core_pending"
	ErrorCoreConfigInvalid  = "core_config_invalid"
	ErrorCorePortConflict   = "core_port_conflict"
	ErrorCoreStartFailed    = "core_start_failed"
	ErrorCoreHealthFailed   = "core_health_failed"
	ErrorCoreRollbackFailed = "core_rollback_failed"
	ErrorCoreRecoveryFailed = "core_recovery_failed"
	ErrorCoreSampleFailed   = "core_sample_failed"
	ErrorCoreStorageFailed  = "core_storage_failed"
)

func IsCoreApplyError(code string) bool {
	switch code {
	case "", ErrorCoreNotImplemented, ErrorCorePending, ErrorCoreConfigInvalid, ErrorCorePortConflict, ErrorCoreStartFailed,
		ErrorCoreHealthFailed, ErrorCoreRollbackFailed, ErrorCoreRecoveryFailed, ErrorCoreSampleFailed, ErrorCoreStorageFailed:
		return true
	}
	return false
}
