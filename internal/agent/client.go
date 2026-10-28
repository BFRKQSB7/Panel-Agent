package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

const maxAgentResponseBytes = 1 << 20

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Client struct {
	store       *identityStore
	identity    Identity
	httpClient  *http.Client
	syncMu      sync.Mutex
	processLock *storage.ProcessLock
	core        *CoreManager
	ledger      *ledger.Store
	hysteria    *hysteriaManager
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Panel API returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("Panel API returned HTTP %d (%s): %s", e.Status, e.Code, e.Message)
}

func NewClient(store *identityStore, identity Identity, caCertPath string) (*Client, error) {
	parsed, err := url.Parse(identity.PanelURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("Panel URL must be an HTTPS origin without credentials or path")
	}
	var roots *x509.CertPool
	if caCertPath != "" {
		pem, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("read Panel CA certificate: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("Panel CA file contains no PEM certificates")
		}
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport does not support TLS configuration")
	}
	transport := defaultTransport.Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	client := &http.Client{
		Transport: transport,
		Timeout:   20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{store: store, identity: identity, httpClient: client}, nil
}

func (c *Client) Close() {
	if c.hysteria != nil {
		c.hysteria.Close()
		c.hysteria = nil
	}
	if c.core != nil && c.core.stopWithParent {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.core.stopTransient(cleanup)
		cancel()
	}
	if c.ledger != nil {
		_ = c.ledger.Close()
		c.ledger = nil
	}
	if transport, ok := c.httpClient.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	if c.processLock != nil {
		_ = c.processLock.Close()
		c.processLock = nil
	}
}

func (c *Client) Identity() Identity {
	return c.identity
}

// EnableCore recovers the local Current/LKG before contacting Panel, so a
// disconnected Agent can restore service using its persisted configuration.
func (c *Client) EnableCore(ctx context.Context, binaryPath string, developmentLoopback bool) error {
	return c.EnableCores(ctx, binaryPath, "", developmentLoopback)
}

func (c *Client) EnableCores(ctx context.Context, binaryPath, hysteriaPath string, developmentLoopback bool, stopWithParent ...bool) error {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if c.core != nil {
		return errors.New("Core manager is already enabled")
	}
	if c.hysteria != nil {
		return errors.New("Hysteria manager is already enabled")
	}
	if binaryPath == "" {
		if hysteriaPath == "" {
			return errors.New("请指定至少一个内核")
		}
		book, err := ledger.Open(c.store.dataDir, c.identity.MachineID)
		if err != nil {
			return err
		}
		c.ledger = book
		c.hysteria, err = newHysteriaManager(ctx, c.store.dataDir, hysteriaPath, book)
		return err
	}
	manager, err := NewCoreManager(c.store.dataDir, binaryPath, developmentLoopback, stopWithParent...)
	if err != nil {
		return err
	}
	book, err := ledger.Open(c.store.dataDir, c.identity.MachineID)
	if err != nil {
		return err
	}
	c.core, c.ledger, manager.ledger = manager, book, book
	if hysteriaPath != "" {
		manager.hysteriaConfigured = true
		c.hysteria, err = newHysteriaManager(ctx, c.store.dataDir, hysteriaPath, book)
		if err != nil {
			return err
		}
	}
	if err := manager.Recover(ctx); err != nil {
		return err
	}
	return manager.ReconcileExpiration(ctx)
}

func (c *Client) CompletePairing(ctx context.Context) error {
	if !c.identity.Pending {
		return nil
	}
	var recovered protocol.RegisterResponse
	status, err := c.postJSON(ctx, "/api/v1/machines/recover", protocol.RecoveryRequest{RegistrationID: c.identity.RegistrationID}, c.identity.MachineSecret, &recovered)
	if err == nil {
		if status != http.StatusOK {
			return fmt.Errorf("unexpected recovery status %d", status)
		}
		return c.acceptRegistration(recovered)
	}
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.Status != http.StatusUnauthorized {
		return err
	}
	request := protocol.RegisterRequest{
		RegistrationID:  c.identity.RegistrationID,
		JoinToken:       c.identity.PendingJoinToken,
		CandidateSecret: c.identity.MachineSecret,
		TargetMachineID: c.identity.TargetMachineID,
		Hostname:        c.identity.Hostname,
		DisplayName:     c.identity.DisplayName,
		OS:              c.identity.OS,
		Architecture:    c.identity.Architecture,
		CPUCount:        c.identity.CPUCount,
		MemoryBytes:     c.identity.MemoryBytes,
		AgentVersion:    protocol.AgentVersion,
	}
	var registered protocol.RegisterResponse
	status, err = c.postJSON(ctx, "/api/v1/machines/register", request, "", &registered)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("unexpected registration status %d", status)
	}
	return c.acceptRegistration(registered)
}

func (c *Client) acceptRegistration(response protocol.RegisterResponse) error {
	if response.RegistrationID != c.identity.RegistrationID || !validUUIDv4(response.MachineID) || !validUUIDv4(response.AuthorityEpoch) || response.Revision == 0 || !validDigest(response.Digest) {
		return errors.New("Panel returned an invalid registration result")
	}
	if c.identity.MachineID != "" && c.identity.MachineID != response.MachineID {
		return errors.New("Panel registration result changed the paired machine identity")
	}
	if response.DesiredState != nil {
		digest, err := protocol.DigestDesiredState(*response.DesiredState)
		if err != nil || digest != response.Digest {
			return errors.New("Panel registration snapshot digest is invalid")
		}
		c.identity.DesiredState = *response.DesiredState
	} else if c.identity.AuthorityEpoch == response.AuthorityEpoch && c.identity.Revision == response.Revision && c.identity.Digest == response.Digest {
		// Keep the last verified snapshot when recovering a re-pair result.
	} else {
		c.identity.DesiredState = protocol.EmptyDesiredState()
	}
	c.identity.MachineID = response.MachineID
	c.identity.AuthorityEpoch = response.AuthorityEpoch
	c.identity.Revision = response.Revision
	c.identity.Digest = response.Digest
	c.identity.AuthorityNonce = ""
	c.identity.Pending = false
	c.identity.PendingJoinToken = ""
	c.identity.TargetMachineID = ""
	return c.store.Save(c.identity)
}

func (c *Client) SyncOnce(ctx context.Context) (syncErr error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if c.hysteria != nil {
		// Keep local expiry/recovery active even while the Panel is unreachable;
		// a local failure must not prevent fetching a corrective snapshot.
		_ = c.hysteria.Reconcile(ctx)
	}
	if c.core != nil {
		defer func() {
			if ctx.Err() == nil {
				syncErr = errors.Join(syncErr, c.core.ReconcileExpiration(ctx))
			}
		}()
	}
	if c.core != nil {
		if err := c.core.ReconcileExpiration(ctx); err != nil {
			return err
		}
	}
	if c.identity.Pending {
		if err := c.CompletePairing(ctx); err != nil {
			return err
		}
	}
	if c.identity.MachineID == "" || c.identity.Pending {
		return errors.New("Agent is not paired")
	}
	for attempt := 0; attempt < 4; attempt++ {
		requestID, err := security.NewUUID()
		if err != nil {
			return err
		}
		request := protocol.DesiredStateRequest{
			RequestID: requestID, AuthorityEpoch: c.identity.AuthorityEpoch, Revision: c.identity.Revision,
			Digest: c.identity.Digest, AuthorityNonce: c.identity.AuthorityNonce,
			FullSnapshotReceived: c.identity.AuthorityNonce != "",
		}
		var response protocol.DesiredStateResponse
		if _, err := c.postJSON(ctx, "/api/v1/agent/desired-state", request, c.identity.MachineSecret, &response); err != nil {
			return err
		}
		if response.RequestID != requestID || !validUUIDv4(response.AuthorityEpoch) || response.Revision == 0 || !validDigest(response.Digest) {
			return errors.New("Panel returned a stale or malformed desired-state response")
		}
		if response.AuthorityChange {
			if c.ledger != nil {
				c.ledger.PauseACKs()
			}
			if response.AuthorityNonce == "" || !validSecret(response.AuthorityNonce) || response.DesiredState == nil {
				return errors.New("Panel authority challenge is incomplete")
			}
			digest, err := protocol.DigestDesiredState(*response.DesiredState)
			if err != nil || digest != response.Digest {
				return errors.New("Panel authority snapshot digest is invalid")
			}
			c.identity.AuthorityEpoch = response.AuthorityEpoch
			c.identity.Revision = response.Revision
			c.identity.Digest = response.Digest
			c.identity.DesiredState = *response.DesiredState
			c.identity.AuthorityNonce = response.AuthorityNonce
			if err := c.store.Save(c.identity); err != nil {
				return err
			}
			continue
		}
		if response.AuthorityEpoch != c.identity.AuthorityEpoch {
			return errors.New("Panel changed authority without a full-snapshot handshake")
		}
		if c.identity.AuthorityNonce != "" {
			if response.AcknowledgedNonce != c.identity.AuthorityNonce {
				return errors.New("Panel did not acknowledge the pending authority handshake")
			}
			c.identity.AuthorityNonce = ""
		} else if response.AcknowledgedNonce != "" {
			return errors.New("Panel acknowledged an unknown authority handshake")
		}
		if response.Unchanged {
			if response.DesiredState != nil || response.Revision != c.identity.Revision || response.Digest != c.identity.Digest {
				return errors.New("Panel unchanged response does not match the persisted snapshot")
			}
		} else {
			if response.DesiredState == nil || response.Revision < c.identity.Revision {
				return errors.New("Panel full snapshot is missing or older than persisted state")
			}
			digest, err := protocol.DigestDesiredState(*response.DesiredState)
			if err != nil || digest != response.Digest {
				return errors.New("Panel full snapshot digest is invalid")
			}
			if response.Revision == c.identity.Revision && response.Digest != c.identity.Digest {
				return errors.New("Panel reused a revision with a different digest")
			}
			c.identity.Revision = response.Revision
			c.identity.Digest = response.Digest
			c.identity.DesiredState = *response.DesiredState
		}
		if err := c.store.Save(c.identity); err != nil {
			return err
		}
		var applyErr error
		if c.core == nil && c.hysteria != nil {
			if c.identity.DesiredState.Exit != nil || len(c.identity.DesiredState.TransitOutbounds) > 0 {
				applyErr = errors.New("该机器缺少 Xray 内核")
			}
			for _, n := range c.identity.DesiredState.Nodes {
				if n.Enabled && (n.Extension == nil || n.Extension.Kind != "hysteria2_tls") {
					applyErr = errors.New("该机器缺少 Xray 内核")
				}
			}
		}
		if c.hysteria != nil {
			if applyErr == nil {
				applyErr = c.hysteria.Apply(ctx, c.identity.AuthorityEpoch, c.identity.Revision, c.identity.Digest, c.identity.DesiredState)
			}
		}
		if c.core != nil {
			if applyErr == nil {
				applyErr = c.core.Apply(ctx, c.identity.AuthorityEpoch, c.identity.Revision, c.identity.Digest, c.identity.DesiredState)
			}
		}
		statusErr := c.reportStatus(ctx)
		var trafficErr error
		if c.core != nil {
			trafficErr = c.core.Sample(ctx)
		}
		if c.hysteria != nil {
			trafficErr = errors.Join(trafficErr, c.hysteria.Sample(ctx))
		}
		if applyErr == nil && statusErr == nil && c.identity.DesiredState.TrafficReportingEnabled {
			trafficErr = errors.Join(trafficErr, c.syncTraffic(ctx))
		}
		var upgradeErr error
		if applyErr == nil && statusErr == nil && trafficErr == nil {
			upgradeErr = c.syncUpgrades(ctx)
		}
		return errors.Join(applyErr, statusErr, trafficErr, upgradeErr)
	}
	return errors.New("Panel authority changed repeatedly during the sync sequence")
}

func (c *Client) reportStatus(ctx context.Context) error {
	requestID, err := security.NewUUID()
	if err != nil {
		return err
	}
	request := protocol.StatusRequest{
		RequestID:      requestID,
		AuthorityEpoch: c.identity.AuthorityEpoch,
		Revision:       c.identity.Revision,
		Digest:         c.identity.Digest,
		Hostname:       c.identity.Hostname,
		DisplayName:    c.identity.DisplayName,
		OS:             c.identity.OS,
		Architecture:   c.identity.Architecture,
		CPUCount:       c.identity.CPUCount,
		MemoryBytes:    c.identity.MemoryBytes,
		AgentVersion:   protocol.AgentVersion,
		Received:       true,
		Applied:        false,
		ApplyError:     protocol.ErrorCoreNotImplemented,
	}
	if c.core != nil {
		status := c.core.Status()
		request.CoreGeneration, request.CoreInstanceID = status.Generation, status.InstanceID
		request.AppliedAuthorityEpoch, request.AppliedRevision, request.AppliedDigest = status.AuthorityEpoch, status.Revision, status.Digest
		request.AppliedRestrictionDigest = status.RestrictionDigest
		if !status.RestrictionAppliedAt.IsZero() {
			request.RestrictionAppliedAt = status.RestrictionAppliedAt.UTC().Format(time.RFC3339Nano)
		}
		request.Applied = status.Running && status.AuthorityEpoch == c.identity.AuthorityEpoch && status.Revision == c.identity.Revision && status.Digest == c.identity.Digest
		if c.hysteria != nil {
			request.Applied = request.Applied && c.hysteria.Matches(c.identity.AuthorityEpoch, c.identity.Revision, c.identity.Digest)
		}
		request.ApplyError = status.Error
		if !request.Applied && request.ApplyError == "" {
			request.ApplyError = protocol.ErrorCorePending
		}
	} else if c.hysteria != nil {
		status := c.hysteria.Status()
		request.CoreGeneration, request.CoreInstanceID = status.Generation, status.InstanceID
		request.AppliedAuthorityEpoch, request.AppliedRevision, request.AppliedDigest = status.AuthorityEpoch, status.Revision, status.Digest
		request.AppliedRestrictionDigest = status.RestrictionDigest
		if !status.RestrictionAppliedAt.IsZero() {
			request.RestrictionAppliedAt = status.RestrictionAppliedAt.UTC().Format(time.RFC3339Nano)
		}
		request.Applied = !requiresXray(c.identity.DesiredState) && c.hysteria.Matches(c.identity.AuthorityEpoch, c.identity.Revision, c.identity.Digest)
		request.ApplyError = status.Error
		if !request.Applied && request.ApplyError == "" {
			request.ApplyError = protocol.ErrorCorePending
		}
	}
	var response protocol.StatusResponse
	if _, err := c.postJSON(ctx, "/api/v1/agent/status", request, c.identity.MachineSecret, &response); err != nil {
		return err
	}
	if !response.Accepted {
		return errors.New("Panel did not accept the machine status")
	}
	if heartbeat, err := time.Parse(time.RFC3339Nano, response.LastHeartbeat); err == nil {
		c.identity.LastHeartbeat = heartbeat
		return c.store.Save(c.identity)
	}
	return errors.New("Panel returned an invalid heartbeat time")
}

func requiresXray(state protocol.DesiredState) bool {
	if state.Exit != nil || len(state.TransitOutbounds) > 0 {
		return true
	}
	for _, node := range state.Nodes {
		if node.Enabled && (node.Extension == nil || node.Extension.Kind != "hysteria2_tls") {
			return true
		}
	}
	return false
}

func (c *Client) postJSON(ctx context.Context, path string, request any, bearer string, response any) (int, error) {
	if c.core != nil {
		if expiry := c.core.NextExpiry(); !expiry.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, expiry)
			defer cancel()
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return 0, fmt.Errorf("encode control message: %w", err)
	}
	endpoint := strings.TrimRight(c.identity.PanelURL, "/") + path
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create control request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	if bearer != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+bearer)
	}
	httpResponse, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return 0, fmt.Errorf("Panel request failed: %w", err)
	}
	defer httpResponse.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxAgentResponseBytes+1))
	if err != nil {
		return httpResponse.StatusCode, fmt.Errorf("read Panel response: %w", err)
	}
	if len(contents) > maxAgentResponseBytes {
		return httpResponse.StatusCode, errors.New("Panel response exceeded the size limit")
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		var details protocol.ErrorResponse
		_ = json.Unmarshal(contents, &details)
		return httpResponse.StatusCode, &APIError{Status: httpResponse.StatusCode, Code: details.Error.Code, Message: details.Error.Message}
	}
	if response != nil && len(contents) > 0 {
		if err := json.Unmarshal(contents, response); err != nil {
			return httpResponse.StatusCode, fmt.Errorf("decode Panel response: %w", err)
		}
	}
	return httpResponse.StatusCode, nil
}

func (c *Client) Run(ctx context.Context) error {
	if c.core != nil || c.hysteria != nil {
		measureCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); c.runSampler(measureCtx) }()
		defer func() { cancel(); <-done }()
	}
	backoff := time.Second
	failed := false
	for {
		err := c.SyncOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := 30*time.Second + jitter(5*time.Second)
		if err != nil {
			if !failed {
				message := err.Error()
				if len(message) > 256 {
					message = message[:256]
				}
				log.Printf("控制通道同步失败，将退避重试：%s", message)
			}
			failed = true
			wait = backoff + jitter(backoff/2)
			if backoff < 60*time.Second {
				backoff *= 2
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
			}
		} else {
			if failed {
				log.Print("控制通道同步已恢复")
			}
			failed = false
			backoff = time.Second
		}
		if c.core != nil {
			if expiry := c.core.NextExpiry(); !expiry.IsZero() {
				if until := time.Until(expiry); until < wait {
					wait = max(until, time.Millisecond)
				}
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func jitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(maximum) + 1))
}

func (c *Client) SaveIdentity() error {
	return c.store.Save(c.identity)
}

func validSecret(value string) bool {
	return security.IsValidToken(value, security.MachineSecretBytes)
}

func validUUIDv4(value string) bool {
	return uuidV4Pattern.MatchString(strings.ToLower(value))
}

func validDigest(value string) bool {
	return security.IsValidDigest(value)
}
