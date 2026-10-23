package control

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
	"panel-agent.local/panel-agent/internal/web"
)

var ErrNotInitialized = errors.New("管理员尚未初始化，请先运行本机 admin init 命令")

type Server struct {
	config      Config
	tlsConfig   *tls.Config
	store       *storage.Store
	processLock *storage.ProcessLock
	sessions    *sessionManager
	limiter     *loginLimiter
	templates   *template.Template
}

func NewServer(config Config) (*Server, error) {
	tlsConfig, err := config.validate()
	if err != nil {
		return nil, err
	}
	dataDir, err := storage.PrepareDataDir(config.DataDir)
	if err != nil {
		return nil, err
	}
	processLock, err := storage.AcquireProcessLock(dataDir)
	if err != nil {
		return nil, err
	}
	if _, err := security.LoadOrCreateMasterKey(dataDir, filepath.Join(dataDir, storage.DatabaseFileName)); err != nil {
		_ = processLock.Close()
		return nil, err
	}
	store, err := storage.Open(dataDir)
	if err != nil {
		_ = processLock.Close()
		return nil, err
	}
	hasAdmin, err := store.HasAdmin(context.Background())
	if err != nil {
		_ = store.Close()
		_ = processLock.Close()
		return nil, err
	}
	if !hasAdmin {
		_ = store.Close()
		_ = processLock.Close()
		return nil, ErrNotInitialized
	}
	templates, err := web.ParseTemplates()
	if err != nil {
		_ = store.Close()
		_ = processLock.Close()
		return nil, fmt.Errorf("load embedded web templates: %w", err)
	}
	return &Server{
		config:      config,
		tlsConfig:   tlsConfig,
		store:       store,
		processLock: processLock,
		sessions:    newSessionManager(),
		limiter:     newLoginLimiter(),
		templates:   templates,
	}, nil
}

func InitializeAdmin(dataDir, passwordHash string) (string, error) {
	absolute, err := storage.PrepareDataDir(dataDir)
	if err != nil {
		return "", err
	}
	processLock, err := storage.AcquireProcessLock(absolute)
	if err != nil {
		return "", err
	}
	defer processLock.Close()
	if _, err := security.LoadOrCreateMasterKey(absolute, filepath.Join(absolute, storage.DatabaseFileName)); err != nil {
		return "", err
	}
	store, err := storage.Open(absolute)
	if err != nil {
		return "", err
	}
	defer store.Close()
	hasAdmin, err := store.HasAdmin(context.Background())
	if err != nil {
		return "", err
	}
	if hasAdmin {
		return "", errors.New("administrator is already initialized")
	}
	return store.InitializeAdmin(context.Background(), passwordHash)
}

func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	var storeErr, lockErr error
	if s.store != nil {
		storeErr = s.store.Close()
	}
	if s.processLock != nil {
		lockErr = s.processLock.Close()
	}
	return errors.Join(storeErr, lockErr)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/admin", s.handleAdmin)
	mux.HandleFunc("/admin/settings", s.handleSettingsForm)
	mux.HandleFunc("GET /admin/machines", s.handleMachinesPage)
	mux.HandleFunc("POST /admin/join-tokens", s.handleCreateJoinToken)
	mux.HandleFunc("POST /admin/machines/{machineID}/details", s.handleMachineDetails)
	mux.HandleFunc("POST /admin/machines/{machineID}/revoke", s.handleMachineRevoke)
	mux.HandleFunc("POST /admin/machines/{machineID}/re-pair-token", s.handleMachineRepairToken)
	mux.HandleFunc("POST /admin/authority/rotate", s.handleAuthorityRotate)
	mux.HandleFunc("/api/settings", s.handleSettingsAPI)
	mux.HandleFunc("POST /api/v1/machines/register", s.handleMachineRegister)
	mux.HandleFunc("POST /api/v1/machines/recover", s.handleMachineRecovery)
	mux.HandleFunc("POST /api/v1/agent/desired-state", s.handleDesiredState)
	mux.HandleFunc("POST /api/v1/agent/status", s.handleMachineStatus)
	return s.securityHeaders(mux)
}

func (s *Server) Serve(ctx context.Context) error {
	if ctx == nil {
		return errors.New("serve context is nil")
	}
	listener, err := net.Listen("tcp", s.config.Listen)
	if err != nil {
		return fmt.Errorf("listen on management address: %w", err)
	}
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		TLSConfig:         s.tlsConfig,
	}
	serveErr := make(chan error, 1)
	go func() {
		if s.tlsConfig != nil {
			serveErr <- server.ServeTLS(listener, "", "")
			return
		}
		serveErr <- server.Serve(listener)
	}()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("shut down management server: %w", err)
		}
		err := <-serveErr
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
