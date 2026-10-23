package control

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

const maxProtocolBodyBytes = 16 << 10

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func (s *Server) handleMachinesPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	machines, err := s.store.ListMachines(r.Context())
	if err != nil {
		http.Error(w, "机器列表暂时不可用。", http.StatusInternalServerError)
		return
	}
	views := make([]protocol.MachineView, 0, len(machines))
	for _, machine := range machines {
		views = append(views, storage.MachineViewFromRecord(machine))
	}
	authority, err := s.store.GetPanelAuthority(r.Context())
	if err != nil {
		http.Error(w, "Panel authority 暂时不可用。", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "machines.html", map[string]any{
		"CSRF": session.csrf, "Machines": views, "Authority": authority,
	}); err != nil {
		return
	}
}

func (s *Server) handleCreateJoinToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	if target := strings.TrimSpace(form.Get("target_machine_id")); target != "" {
		s.issueJoinTokenPage(w, r, session, target)
		return
	}
	s.issueJoinTokenPage(w, r, session, "")
}

func (s *Server) handleMachineRepairToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	target := r.PathValue("machineID")
	if !validUUIDv4(target) {
		http.NotFound(w, r)
		return
	}
	s.issueJoinTokenPage(w, r, session, target)
}

func (s *Server) issueJoinTokenPage(w http.ResponseWriter, r *http.Request, session sessionRecord, target string) {
	token, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		http.Error(w, "无法签发接入凭据。", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	expires := now.Add(30 * time.Minute)
	if err := s.store.CreateJoinToken(r.Context(), security.HashToken(token), target, expires, now); err != nil {
		if errors.Is(err, storage.ErrMachineNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "无法保存接入凭据。", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "join-token.html", map[string]any{
		"CSRF": session.csrf, "JoinToken": token, "ExpiresAt": expires.Format(time.RFC3339), "TargetMachineID": target,
	}); err != nil {
		return
	}
}

func (s *Server) handleMachineDetails(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	displayName := strings.TrimSpace(form.Get("display_name"))
	notes := strings.TrimSpace(form.Get("notes"))
	if !validHumanText(displayName, 128, false) || !validHumanText(notes, 1024, true) {
		http.Error(w, "机器名称或备注无效。", http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateMachineDetails(r.Context(), r.PathValue("machineID"), displayName, notes, time.Now()); err != nil {
		if errors.Is(err, storage.ErrMachineNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "无法保存机器信息。", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/machines", http.StatusSeeOther)
}

func (s *Server) handleMachineRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	if err := s.store.RevokeMachine(r.Context(), r.PathValue("machineID"), time.Now()); err != nil {
		if errors.Is(err, storage.ErrMachineNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "无法吊销机器凭据。", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/machines", http.StatusSeeOther)
}

func (s *Server) handleAuthorityRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	if _, err := s.store.RotatePanelAuthority(r.Context(), time.Now()); err != nil {
		http.Error(w, "无法生成新的随机 authority。", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/machines", http.StatusSeeOther)
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (sessionRecord, bool) {
	session, ok := s.requestSession(r)
	if !ok {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSONError(w, http.StatusUnauthorized, "尚未登录")
		} else {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}
		return sessionRecord{}, false
	}
	return session, true
}

func (s *Server) handleMachineRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, protocol.ErrorInvalidMessage, "仅支持 POST 请求")
		return
	}
	if !s.requireAgentHTTPS(w, r) {
		return
	}
	var request protocol.RegisterRequest
	if err := decodeProtocolJSON(w, r, &request); err != nil {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "注册消息格式无效")
		return
	}
	if !validUUIDv4(request.RegistrationID) || !validSecret(request.JoinToken) || !validSecret(request.CandidateSecret) ||
		(request.TargetMachineID != "" && !validUUIDv4(request.TargetMachineID)) ||
		!validHumanText(request.Hostname, 255, false) || !validHumanText(request.DisplayName, 128, true) ||
		!validHumanText(request.OS, 64, false) || !validHumanText(request.Architecture, 64, false) ||
		request.CPUCount < 0 || request.CPUCount > 4096 || request.MemoryBytes > 1<<60 || !validHumanText(request.AgentVersion, 64, false) {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "注册信息无效")
		return
	}
	machine, _, err := s.store.RegisterMachine(r.Context(), storage.RegisterMachineInput{
		RegistrationID: request.RegistrationID, JoinTokenHash: security.HashToken(request.JoinToken),
		CandidateHash: security.HashToken(request.CandidateSecret), TargetMachineID: request.TargetMachineID,
		Hostname: strings.TrimSpace(request.Hostname), DisplayName: strings.TrimSpace(request.DisplayName),
		OS: strings.TrimSpace(request.OS), Architecture: strings.TrimSpace(request.Architecture),
		CPUCount: request.CPUCount, MemoryBytes: request.MemoryBytes, AgentVersion: strings.TrimSpace(request.AgentVersion),
	}, time.Now())
	if err != nil {
		if writeMachineError(w, err) {
			return
		}
		writeProtocolError(w, http.StatusInternalServerError, protocol.ErrorInvalidMessage, "注册请求暂时无法完成")
		return
	}
	response, err := s.machineRegistrationResponse(r, machine)
	if err != nil {
		writeProtocolError(w, http.StatusInternalServerError, protocol.ErrorInvalidMessage, "无法读取机器初始状态")
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) handleMachineRecovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, protocol.ErrorInvalidMessage, "仅支持 POST 请求")
		return
	}
	if !s.requireAgentHTTPS(w, r) {
		return
	}
	var request protocol.RecoveryRequest
	if err := decodeProtocolJSON(w, r, &request); err != nil || !validUUIDv4(request.RegistrationID) {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "恢复消息格式无效")
		return
	}
	token, ok := bearerSecret(r)
	if !ok {
		writeProtocolError(w, http.StatusUnauthorized, protocol.ErrorAuthenticationRequired, "机器认证失败")
		return
	}
	machine, err := s.store.RecoverRegistration(r.Context(), request.RegistrationID, security.HashToken(token))
	if err != nil {
		if !writeMachineError(w, err) {
			writeProtocolError(w, http.StatusInternalServerError, protocol.ErrorInvalidMessage, "恢复请求暂时无法完成")
		}
		return
	}
	response, err := s.machineRegistrationResponse(r, machine)
	if err != nil {
		writeProtocolError(w, http.StatusInternalServerError, protocol.ErrorInvalidMessage, "无法读取机器初始状态")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) machineRegistrationResponse(r *http.Request, machine storage.MachineRecord) (protocol.RegisterResponse, error) {
	response := protocol.RegisterResponse{
		MachineID: machine.MachineID, RegistrationID: machine.RegistrationID,
		AuthorityEpoch: machine.AuthorityEpoch, Revision: machine.DesiredRevision, Digest: machine.DesiredDigest,
	}
	current, err := s.store.GetMachineDesiredState(r.Context(), machine.MachineID)
	if err != nil {
		return protocol.RegisterResponse{}, err
	}
	if machine.AuthorityEpoch == current.Epoch && machine.DesiredRevision == current.Revision && machine.DesiredDigest == current.Digest {
		snapshot := current.Snapshot
		response.DesiredState = &snapshot
	}
	return response, nil
}

func (s *Server) handleDesiredState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, protocol.ErrorInvalidMessage, "仅支持 POST 请求")
		return
	}
	machine, ok := s.authenticatedAgent(w, r)
	if !ok {
		return
	}
	var request protocol.DesiredStateRequest
	if err := decodeProtocolJSON(w, r, &request); err != nil || !validUUIDv4(request.RequestID) || !validDigest(request.Digest) || len(request.AuthorityEpoch) > 64 {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "desired-state 消息格式无效")
		return
	}
	response, err := s.store.DesiredStateExchange(r.Context(), machine.MachineID, request)
	if err != nil {
		if !writeMachineError(w, err) {
			writeProtocolError(w, http.StatusInternalServerError, protocol.ErrorInvalidMessage, "无法读取配置快照")
		}
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleMachineStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, protocol.ErrorInvalidMessage, "仅支持 POST 请求")
		return
	}
	machine, ok := s.authenticatedAgent(w, r)
	if !ok {
		return
	}
	var request protocol.StatusRequest
	if err := decodeProtocolJSON(w, r, &request); err != nil || !validUUIDv4(request.RequestID) || !validDigest(request.Digest) || len(request.AuthorityEpoch) > 64 {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "status 消息格式无效")
		return
	}
	lastHeartbeat, err := s.store.RecordMachineStatus(r.Context(), machine.MachineID, request, time.Now())
	if err != nil {
		if !writeMachineError(w, err) {
			writeProtocolError(w, http.StatusInternalServerError, protocol.ErrorInvalidMessage, "无法保存机器状态")
		}
		return
	}
	writeJSON(w, http.StatusOK, protocol.StatusResponse{Accepted: true, LastHeartbeat: lastHeartbeat})
}

func (s *Server) authenticatedAgent(w http.ResponseWriter, r *http.Request) (storage.MachineRecord, bool) {
	if !s.requireAgentHTTPS(w, r) {
		return storage.MachineRecord{}, false
	}
	token, ok := bearerSecret(r)
	if !ok {
		writeProtocolError(w, http.StatusUnauthorized, protocol.ErrorAuthenticationRequired, "机器认证失败")
		return storage.MachineRecord{}, false
	}
	machine, err := s.store.AuthenticateMachine(r.Context(), security.HashToken(token))
	if err != nil {
		writeProtocolError(w, http.StatusUnauthorized, protocol.ErrorAuthenticationRequired, "机器认证失败")
		return storage.MachineRecord{}, false
	}
	return machine, true
}

func (s *Server) requireAgentHTTPS(w http.ResponseWriter, r *http.Request) bool {
	if s.requestScheme(r) == "https" {
		return true
	}
	writeProtocolError(w, http.StatusUpgradeRequired, protocol.ErrorInvalidMessage, "Agent 控制通道必须使用 HTTPS")
	return false
}

func bearerSecret(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if !validSecret(token) {
		return "", false
	}
	return token, true
}

func decodeProtocolJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("application/json is required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProtocolBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func validSecret(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == security.MachineSecretBytes
}

func validUUIDv4(value string) bool {
	return uuidV4Pattern.MatchString(strings.ToLower(value))
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func validHumanText(value string, maxRunes int, allowEmpty bool) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return allowEmpty
	}
	if len([]rune(value)) > maxRunes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func writeMachineError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, storage.ErrInvalidJoinToken):
		writeProtocolError(w, http.StatusUnauthorized, protocol.ErrorInvalidJoinToken, "接入凭据无效")
	case errors.Is(err, storage.ErrExpiredJoinToken):
		writeProtocolError(w, http.StatusGone, protocol.ErrorExpiredJoinToken, "接入凭据已过期")
	case errors.Is(err, storage.ErrUsedJoinToken):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorUsedJoinToken, "接入凭据已使用")
	case errors.Is(err, storage.ErrRegistrationConflict):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorRegistrationConflict, "注册身份已被使用，请先执行恢复查询")
	case errors.Is(err, storage.ErrMachineAuthentication):
		writeProtocolError(w, http.StatusUnauthorized, protocol.ErrorAuthenticationRequired, "机器认证失败")
	case errors.Is(err, storage.ErrMachineNotFound):
		writeProtocolError(w, http.StatusNotFound, protocol.ErrorMachineNotFound, "机器不存在")
	case errors.Is(err, storage.ErrAuthorityMismatch):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorAuthorityMismatch, "配置 authority 已变化")
	case errors.Is(err, storage.ErrStaleRevision):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorStaleRevision, "配置 revision 无效")
	case errors.Is(err, storage.ErrDigestMismatch):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorDigestMismatch, "配置 digest 与 revision 不匹配")
	case errors.Is(err, storage.ErrCoreNotImplemented):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorCoreNotImplemented, "当前 Agent 尚未实现 Core 应用")
	case errors.Is(err, storage.ErrDesiredStateNotReceived):
		writeProtocolError(w, http.StatusConflict, protocol.ErrorDesiredStateNotReceived, "desired-state 尚未接收")
	default:
		return false
	}
	return true
}

func writeProtocolError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, protocol.ErrorResponse{Error: protocol.ErrorDetails{Code: code, Message: message}})
}
