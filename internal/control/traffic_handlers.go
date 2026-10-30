package control

import (
	"errors"
	"net/http"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/storage"
)

const maxTrafficBodyBytes = 4 << 20

func (s *Server) handleTrafficRetentionAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requestSession(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "尚未登录")
		return
	}
	switch r.Method {
	case http.MethodGet:
		retention, err := s.store.GetTrafficRetention(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "无法读取流量保留设置")
			return
		}
		writeJSON(w, http.StatusOK, retention)
	case http.MethodPut:
		if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
			writeJSONError(w, http.StatusForbidden, "请求校验失败")
			return
		}
		var retention storage.TrafficRetention
		if decodeLimitedJSON(w, r, &retention, 4096) != nil || retention.Validate() != nil {
			writeJSONError(w, http.StatusBadRequest, "流量保留设置无效")
			return
		}
		if err := s.store.SaveTrafficRetention(r.Context(), retention); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "无法保存流量保留设置")
			return
		}
		writeJSON(w, http.StatusOK, retention)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
	}
}

func (s *Server) handleTrafficRecovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, protocol.ErrorInvalidMessage, "traffic recovery requires POST")
		return
	}
	machine, ok := s.authenticatedAgent(w, r)
	if !ok {
		return
	}
	var request protocol.TrafficRecoveryRequest
	if err := decodeLimitedJSON(w, r, &request, 64<<10); err != nil {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "invalid traffic recovery request")
		return
	}
	if request.MachineID != machine.MachineID {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "traffic machine identity does not match credentials")
		return
	}
	response, err := s.store.RecoverTraffic(r.Context(), machine.MachineID, request, time.Now().UTC())
	if err != nil {
		if s.writeTrafficError(w, err) {
			return
		}
		writeProtocolError(w, http.StatusInternalServerError, protocol.ErrorInvalidMessage, "traffic recovery failed")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleTrafficIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, protocol.ErrorInvalidMessage, "traffic ingest requires POST")
		return
	}
	machine, ok := s.authenticatedAgent(w, r)
	if !ok {
		return
	}
	var wire protocol.TrafficWireRequest
	if err := decodeLimitedJSON(w, r, &wire, maxTrafficBodyBytes); err != nil {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "invalid traffic request")
		return
	}
	request, rows, err := wire.DecodeRows()
	if err != nil || request.MachineID != machine.MachineID {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "invalid traffic request envelope")
		return
	}
	ack, err := s.store.IngestTraffic(r.Context(), machine.MachineID, request, rows, time.Now().UTC())
	if err != nil {
		if s.writeTrafficError(w, err) {
			return
		}
		writeProtocolError(w, http.StatusInternalServerError, protocol.ErrorInvalidMessage, "traffic ingest failed")
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

func (s *Server) writeTrafficError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, storage.ErrMachineAuthentication):
		writeProtocolError(w, http.StatusUnauthorized, protocol.ErrorAuthenticationRequired, "machine authentication failed")
	case errors.Is(err, storage.ErrAuthorityMismatch):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorAuthorityMismatch, "traffic authority does not match")
	case errors.Is(err, storage.ErrTrafficInvalid):
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "invalid traffic state")
	case errors.Is(err, storage.ErrTrafficEpoch):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorTrafficState, "traffic ledger epoch is not accepted")
	case errors.Is(err, storage.ErrTrafficReceipt):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorTrafficReceipt, "traffic receipt is not accepted")
	case errors.Is(err, storage.ErrTrafficRecoveryGap):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorTrafficRecoveryGap, "traffic recovery has an unproven gap")
	default:
		return false
	}
	return true
}
