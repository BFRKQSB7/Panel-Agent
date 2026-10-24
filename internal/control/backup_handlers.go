package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/backup"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

type backupRequest struct {
	Password string `json:"password"`
}

type trafficGapRequest struct {
	LedgerEpoch    string                 `json:"ledger_epoch"`
	AgentAllocated uint64                 `json:"agent_allocated,string"`
	FirstSeq       uint64                 `json:"first_seq,string"`
	LastSeq        uint64                 `json:"last_seq,string"`
	Held           []protocol.CloseRange  `json:"held_final_ranges"`
	Receipt        security.ClosedReceipt `json:"receipt"`
}

func (s *Server) createBackup(ctx context.Context, password string) ([]byte, backup.Manifest, error) {
	if len([]byte(password)) > backup.MaxPasswordBytes {
		return nil, backup.Manifest{}, errors.New("备份密码过长")
	}
	snapshot, err := s.store.Snapshot(ctx)
	if err != nil {
		return nil, backup.Manifest{}, err
	}
	encoded, manifest, err := backup.Create(backup.Input{
		Database:       snapshot.Database,
		MasterKey:      snapshot.MasterKey,
		SchemaVersion:  snapshot.SchemaVersion,
		InstallationID: snapshot.InstallationID,
		Config: backup.ConfigSnapshot{
			DataDir: s.store.DataDir(), Listen: s.config.Listen, TLSCertPath: s.config.TLSCert,
			TLSKeyPath: s.config.TLSKey, TrustProxy: s.config.TrustProxy, CookieSecure: s.config.CookieSecure,
		},
		Extensions: snapshot.Extensions,
		Password:   password,
	},
	)
	return encoded, manifest, err
}

func (s *Server) handleBackupExport(w http.ResponseWriter, r *http.Request) {
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
	s.writeBackupDownload(w, r, form.Get("password"))
}

func (s *Server) handleBackupExportAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "请求校验失败")
		return
	}
	var input backupRequest
	if err := decodeLimitedJSON(w, r, &input, 8<<10); err != nil {
		writeJSONError(w, http.StatusBadRequest, "备份请求格式无效")
		return
	}
	s.writeBackupDownload(w, r, input.Password)
}

func (s *Server) writeBackupDownload(w http.ResponseWriter, r *http.Request, password string) {
	contents, manifest, err := s.createBackup(r.Context(), password)
	if err != nil {
		http.Error(w, "无法生成一致性备份："+err.Error(), http.StatusInternalServerError)
		return
	}
	filename := "panel-agent-backup-" + strings.ReplaceAll(strings.TrimSuffix(manifest.CreatedAt, "Z"), ":", "-") + ".json"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(contents)))
	_, _ = w.Write(contents)
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	bundle, err := parseMultipartBundle(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, bundle.csrf) {
		http.Error(w, "恢复请求格式或 CSRF 校验失败", http.StatusBadRequest)
		return
	}
	decoded, err := backup.Open(bundle.contents, bundle.password)
	if err != nil {
		http.Error(w, "备份验证失败："+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.restoreBundle(r.Context(), decoded); err != nil {
		http.Error(w, "恢复失败："+err.Error(), http.StatusConflict)
		return
	}
	s.clearSessionCookies(w)
	http.Redirect(w, r, "/login?restored=1", http.StatusSeeOther)
}

func (s *Server) handleBackupRestoreAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Header.Get("X-CSRF-Token") == "" || !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "请求校验失败")
		return
	}
	bundle, err := parseMultipartBundle(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "恢复请求格式无效")
		return
	}
	decoded, err := backup.Open(bundle.contents, bundle.password)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "备份验证失败："+err.Error())
		return
	}
	manifest, err := s.restoreBundle(r.Context(), decoded)
	if err != nil {
		writeJSONError(w, http.StatusConflict, "恢复失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": true, "installation_id": manifest.InstallationID, "authority_rotated": true, "traffic_reconciliation": "pending"})
}

type multipartBundle struct {
	contents []byte
	password string
	csrf     string
}

func parseMultipartBundle(w http.ResponseWriter, r *http.Request) (multipartBundle, error) {
	limit := int64(backup.MaxBundleBytes) + 1<<20
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(limit); err != nil {
		return multipartBundle{}, err
	}
	file, _, err := r.FormFile("backup")
	if err != nil {
		return multipartBundle{}, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, int64(backup.MaxBundleBytes)+1))
	if err != nil || len(contents) > backup.MaxBundleBytes {
		return multipartBundle{}, errors.New("备份包过大")
	}
	return multipartBundle{contents: contents, password: r.FormValue("password"), csrf: r.FormValue("csrf")}, nil
}

func (s *Server) restoreBundle(ctx context.Context, decoded backup.Bundle) (backup.Manifest, error) {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	if s.restoring.Load() {
		return backup.Manifest{}, errors.New("Panel 已在恢复中")
	}
	s.restoring.Store(true)
	s.requestGate.Lock()
	defer func() {
		s.restoring.Store(false)
		s.requestGate.Unlock()
	}()
	oldStore := s.store
	dataDir := oldStore.DataDir()
	expectedInstallation, err := oldStore.InstallationID(ctx)
	if err != nil {
		return backup.Manifest{}, err
	}
	stage, err := backup.Stage(ctx, dataDir, decoded, expectedInstallation)
	if err != nil {
		return backup.Manifest{}, err
	}
	oldActions := s.actions
	if err := oldStore.Close(); err != nil {
		_ = os.RemoveAll(stage.Path)
		return backup.Manifest{}, err
	}
	if err := backup.Swap(dataDir, stage.Path); err != nil {
		return backup.Manifest{}, errors.Join(err, reopenStoreAfterRestore(s, dataDir))
	}
	newStore, err := storage.Open(dataDir)
	if err != nil {
		return rollbackRestore(s, dataDir, stage.Path, err)
	}
	if _, err := newStore.FinalizeRestore(ctx, time.Now().UTC()); err != nil {
		_ = newStore.Close()
		return rollbackRestore(s, dataDir, stage.Path, err)
	}
	master, err := security.LoadOrCreateMasterKey(dataDir, filepath.Join(dataDir, storage.DatabaseFileName))
	if err != nil {
		_ = newStore.Close()
		return rollbackRestore(s, dataDir, stage.Path, err)
	}
	newActions, err := security.NewActionSigner(master)
	clear(master)
	if err != nil {
		_ = newStore.Close()
		return rollbackRestore(s, dataDir, stage.Path, err)
	}
	if err := backup.Commit(dataDir); err != nil {
		newActions.Close()
		_ = newStore.Close()
		return rollbackRestore(s, dataDir, stage.Path, err)
	}
	s.store = newStore
	s.actions = newActions
	s.sessions = newSessionManager()
	oldActions.Close()
	return decoded.Manifest, nil
}

func rollbackRestore(s *Server, dataDir, stage string, cause error) (backup.Manifest, error) {
	rollbackErr := backup.Rollback(dataDir)
	_ = os.RemoveAll(stage)
	openErr := reopenStoreAfterRestore(s, dataDir)
	return backup.Manifest{}, errors.Join(cause, rollbackErr, openErr)
}

func reopenStoreAfterRestore(s *Server, dataDir string) error {
	store, err := storage.Open(dataDir)
	if err != nil {
		return err
	}
	s.store = store
	return nil
}

func (s *Server) handleTrafficRecoveryGapAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "请求校验失败")
		return
	}
	var request trafficGapRequest
	if err := decodeProtocolJSON(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "对账缺口请求格式无效")
		return
	}
	if err := s.store.RecordTrafficRecoveryGap(r.Context(), storage.TrafficRecoveryGapInput{MachineID: r.PathValue("machineID"), LedgerEpoch: request.LedgerEpoch, AgentAllocated: request.AgentAllocated, FirstSeq: request.FirstSeq, LastSeq: request.LastSeq, Held: request.Held, Receipt: request.Receipt}, time.Now().UTC()); err != nil {
		writeTrafficGapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recorded": true, "bytes_added": "0"})
}

func (s *Server) handleTrafficRecoveryGapForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readFormWithLimit(w, r, 64<<10)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	request, err := parseTrafficGapForm(form)
	if err != nil {
		http.Error(w, "对账缺口请求格式无效："+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.RecordTrafficRecoveryGap(r.Context(), storage.TrafficRecoveryGapInput{MachineID: r.PathValue("machineID"), LedgerEpoch: request.LedgerEpoch, AgentAllocated: request.AgentAllocated, FirstSeq: request.FirstSeq, LastSeq: request.LastSeq, Held: request.Held, Receipt: request.Receipt}, time.Now().UTC()); err != nil {
		http.Error(w, "不能登记对账缺口："+err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/admin/machines?reconciled=1", http.StatusSeeOther)
}

func parseTrafficGapForm(form url.Values) (trafficGapRequest, error) {
	parseUint := func(name string) (uint64, error) {
		value := strings.TrimSpace(form.Get(name))
		if value == "" {
			return 0, fmt.Errorf("缺少 %s", name)
		}
		return strconv.ParseUint(value, 10, 63)
	}
	var request trafficGapRequest
	request.LedgerEpoch = strings.TrimSpace(form.Get("ledger_epoch"))
	var err error
	if request.AgentAllocated, err = parseUint("agent_allocated"); err != nil {
		return trafficGapRequest{}, err
	}
	if request.FirstSeq, err = parseUint("first_seq"); err != nil {
		return trafficGapRequest{}, err
	}
	if request.LastSeq, err = parseUint("last_seq"); err != nil {
		return trafficGapRequest{}, err
	}
	if held := strings.TrimSpace(form.Get("held_ranges_json")); held != "" {
		if err := json.Unmarshal([]byte(held), &request.Held); err != nil {
			return trafficGapRequest{}, err
		}
	}
	if receipt := strings.TrimSpace(form.Get("receipt_json")); receipt != "" {
		if err := json.Unmarshal([]byte(receipt), &request.Receipt); err != nil {
			return trafficGapRequest{}, err
		}
	} else {
		return trafficGapRequest{}, errors.New("缺少 receipt_json")
	}
	return request, nil
}

func writeTrafficGapError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrTrafficGapEvidence), errors.Is(err, storage.ErrTrafficGapRange), errors.Is(err, storage.ErrTrafficGapHeld):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, storage.ErrTrafficGapNotPending), errors.Is(err, storage.ErrTrafficGapHasDetails), errors.Is(err, storage.ErrTrafficGapOverlap), errors.Is(err, storage.ErrTrafficEpoch):
		writeJSONError(w, http.StatusConflict, err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "无法登记对账缺口")
	}
}
