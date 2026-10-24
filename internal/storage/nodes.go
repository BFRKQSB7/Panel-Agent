package storage

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

var (
	ErrInvalidNode      = errors.New("节点配置无效")
	ErrNodeNotFound     = errors.New("节点不存在")
	ErrNodePortConflict = errors.New("该机器的监听端口已由其他节点使用")
)

type NodeInput struct {
	Name          string          `json:"name"`
	PublicHost    string          `json:"public_host"`
	PublicPort    int             `json:"public_port"`
	ListenHost    string          `json:"listen_host"`
	ListenPort    int             `json:"listen_port"`
	Enabled       bool            `json:"enabled"`
	ServerName    string          `json:"server_name"`
	Target        string          `json:"target"`
	Development   bool            `json:"development"`
	QuotaBytes    *quantity.Bytes `json:"quota_bytes"`
	ExitMachineID string          `json:"exit_machine_id,omitempty"`
}

type NodeRecord struct {
	NodeID    string `json:"node_id"`
	MachineID string `json:"machine_id"`
	NodeInput
	PublicKey       string `json:"public_key"`
	ShortID         string `json:"short_id"`
	DevelopmentUUID string `json:"development_uuid,omitempty"`
	privateKey      []byte
}

func normalizeNode(input NodeInput) (NodeInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if !protocol.ValidText(input.Name, 128) || input.QuotaBytes != nil && *input.QuotaBytes < 0 {
		return input, fmt.Errorf("%w: 名称不能为空或包含控制字符", ErrInvalidNode)
	}
	var err error
	input.PublicHost, err = protocol.NormalizeHost(input.PublicHost)
	if err != nil || input.PublicPort < 1 || input.PublicPort > 65535 {
		return input, fmt.Errorf("%w: 公网主机或端口无效", ErrInvalidNode)
	}
	ip := net.ParseIP(strings.TrimSpace(input.ListenHost))
	if ip == nil || input.ListenPort < 1 || input.ListenPort > 65535 || input.ListenPort == protocol.CoreStatsPort {
		return input, fmt.Errorf("%w: 监听地址或端口无效，10085 为本机统计端口", ErrInvalidNode)
	}
	input.ListenHost = ip.String()
	input.ServerName, err = protocol.NormalizeHost(input.ServerName)
	if err != nil || net.ParseIP(input.ServerName) != nil {
		return input, fmt.Errorf("%w: Reality SNI 必须是域名", ErrInvalidNode)
	}
	if strings.TrimSpace(input.Target) == "" {
		input.Target = net.JoinHostPort(input.ServerName, "443")
	}
	host, portText, err := net.SplitHostPort(strings.TrimSpace(input.Target))
	if err != nil {
		return input, fmt.Errorf("%w: Reality target 必须包含主机和端口", ErrInvalidNode)
	}
	host, err = protocol.NormalizeHost(host)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || port < 1 || port > 65535 {
		return input, fmt.Errorf("%w: Reality target 无效", ErrInvalidNode)
	}
	input.Target = net.JoinHostPort(host, strconv.Itoa(port))
	return input, nil
}

const nodeSelect = `SELECT node_id, machine_id, name, public_host, public_port, listen_host, listen_port,
 enabled, server_name, reality_target, private_key, public_key, short_id, development, quota_bytes, exit_machine_id FROM nodes`

func scanNode(scanner rowScanner) (NodeRecord, error) {
	var node NodeRecord
	var quota sql.NullInt64
	var exitMachine sql.NullString
	err := scanner.Scan(&node.NodeID, &node.MachineID, &node.Name, &node.PublicHost, &node.PublicPort, &node.ListenHost,
		&node.ListenPort, &node.Enabled, &node.ServerName, &node.Target, &node.privateKey, &node.PublicKey, &node.ShortID, &node.Development, &quota, &exitMachine)
	if err == nil && quota.Valid {
		value := quantity.Bytes(quota.Int64)
		node.QuotaBytes = &value
	}
	if err == nil {
		node.ExitMachineID = exitMachine.String
	}
	return node, err
}

func listNodes(ctx context.Context, query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, machineID string) ([]NodeRecord, error) {
	rows, err := query.QueryContext(ctx, nodeSelect+" WHERE machine_id = ? AND deleted_at IS NULL ORDER BY node_id", machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := []NodeRecord{}
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (s *Store) ListNodes(ctx context.Context, machineID string) ([]NodeRecord, error) {
	if _, err := s.GetMachine(ctx, machineID); err != nil {
		return nil, err
	}
	nodes, err := listNodes(ctx, s.db, machineID)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		if nodes[i].Development {
			nodes[i].DevelopmentUUID, err = s.readDevelopmentUUID(ctx, s.db)
			if err != nil {
				return nil, err
			}
		}
	}
	return nodes, nil
}

func (s *Store) readDevelopmentUUID(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	var encrypted []byte
	if err := query.QueryRowContext(ctx, "SELECT encrypted_uuid FROM development_credential WHERE id = 1").Scan(&encrypted); err != nil {
		return "", err
	}
	plain, err := security.OpenSecret(s.masterKey, encrypted, "development-credential")
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *Store) ensureDevelopmentUUID(ctx context.Context, tx *sql.Tx) error {
	_, err := s.readDevelopmentUUID(ctx, tx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	uuid, err := security.NewUUID()
	if err != nil {
		return err
	}
	encrypted, err := security.SealSecret(s.masterKey, []byte(uuid), "development-credential")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO development_credential(id, encrypted_uuid) VALUES(1, ?)", encrypted)
	return err
}

func (s *Store) SaveNode(ctx context.Context, machineID, nodeID string, input NodeInput, now time.Time) (NodeRecord, error) {
	input, err := normalizeNode(input)
	if err != nil {
		return NodeRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeRecord{}, err
	}
	defer tx.Rollback()
	machine, err := getMachineTx(ctx, tx, machineID)
	if err != nil {
		return NodeRecord{}, err
	}
	if machine.Revoked {
		return NodeRecord{}, ErrMachineAuthentication
	}
	creating := nodeID == ""
	var node NodeRecord
	if creating {
		nodeID, err = security.NewUUID()
		if err != nil {
			return NodeRecord{}, err
		}
		key, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return NodeRecord{}, err
		}
		node.privateKey, err = security.SealSecret(s.masterKey, []byte(base64.RawURLEncoding.EncodeToString(key.Bytes())), "node-key:"+nodeID)
		if err != nil {
			return NodeRecord{}, err
		}
		node.PublicKey = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
		short := make([]byte, 8)
		if _, err := rand.Read(short); err != nil {
			return NodeRecord{}, err
		}
		node.ShortID = hex.EncodeToString(short)
	} else {
		node, err = scanNode(tx.QueryRowContext(ctx, nodeSelect+" WHERE machine_id = ? AND node_id = ? AND deleted_at IS NULL", machineID, nodeID))
		if errors.Is(err, sql.ErrNoRows) {
			return NodeRecord{}, ErrNodeNotFound
		}
		if err != nil {
			return NodeRecord{}, err
		}
	}
	var conflicts int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE machine_id = ? AND listen_port = ? AND node_id != ? AND deleted_at IS NULL", machineID, input.ListenPort, nodeID).Scan(&conflicts); err != nil {
		return NodeRecord{}, err
	}
	if conflicts != 0 {
		return NodeRecord{}, ErrNodePortConflict
	}
	if input.Development {
		if err := s.ensureDevelopmentUUID(ctx, tx); err != nil {
			return NodeRecord{}, err
		}
		node.DevelopmentUUID, err = s.readDevelopmentUUID(ctx, tx)
		if err != nil {
			return NodeRecord{}, err
		}
	}
	if input.ExitMachineID != "" {
		if !security.IsUUIDv4(input.ExitMachineID) || input.ExitMachineID == machineID {
			return NodeRecord{}, ErrInvalidExit
		}
		var exitCount int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM exits WHERE machine_id = ?", input.ExitMachineID).Scan(&exitCount); err != nil {
			return NodeRecord{}, err
		}
		if exitCount != 1 {
			return NodeRecord{}, ErrExitNotFound
		}
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	if creating {
		_, err = tx.ExecContext(ctx, `INSERT INTO nodes(node_id, machine_id, name, public_host, public_port, listen_host, listen_port,
		enabled, server_name, reality_target, private_key, public_key, short_id, development, quota_bytes, exit_machine_id, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, nodeID, machineID, input.Name, input.PublicHost, input.PublicPort,
			input.ListenHost, input.ListenPort, input.Enabled, input.ServerName, input.Target, node.privateKey, node.PublicKey, node.ShortID, input.Development, input.QuotaBytes, nullableString(input.ExitMachineID), nowText, nowText)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE nodes SET name = ?, public_host = ?, public_port = ?, listen_host = ?, listen_port = ?,
		enabled = ?, server_name = ?, reality_target = ?, development = ?, quota_bytes = ?, exit_machine_id = ?, updated_at = ? WHERE machine_id = ? AND node_id = ? AND deleted_at IS NULL`,
			input.Name, input.PublicHost, input.PublicPort, input.ListenHost, input.ListenPort, input.Enabled, input.ServerName, input.Target, input.Development, input.QuotaBytes, nullableString(input.ExitMachineID), nowText, machineID, nodeID)
	}
	if err != nil {
		return NodeRecord{}, fmt.Errorf("persist Node: %w", err)
	}
	if err := s.refreshMachineDesiredTx(ctx, tx, machineID); err != nil {
		return NodeRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeRecord{}, err
	}
	node.NodeID, node.MachineID, node.NodeInput = nodeID, machineID, input
	return node, nil
}

func (s *Store) DeleteNode(ctx context.Context, machineID, nodeID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Tombstones keep Node identities available to later accounting reconciliation.
	result, err := tx.ExecContext(ctx, "UPDATE nodes SET deleted_at = ?, enabled = 0 WHERE machine_id = ? AND node_id = ? AND deleted_at IS NULL", now.UTC().Format(time.RFC3339Nano), machineID, nodeID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNodeNotFound
	}
	if err := s.refreshMachineDesiredTx(ctx, tx, machineID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) refreshMachineDesiredTx(ctx context.Context, tx *sql.Tx, machineID string) error {
	return s.refreshMachineDesiredTxMode(ctx, tx, machineID, true)
}

func (s *Store) refreshMachineDesiredTxMode(ctx context.Context, tx *sql.Tx, machineID string, syncTransit bool) error {
	changedExitMachines := []string{}
	if syncTransit {
		var err error
		changedExitMachines, err = s.syncTransitCredentialsTx(ctx, tx)
		if err != nil {
			return err
		}
	}
	for _, exitMachineID := range changedExitMachines {
		if exitMachineID == machineID {
			continue
		}
		if err := s.refreshMachineDesiredTxMode(ctx, tx, exitMachineID, false); err != nil {
			return err
		}
	}
	nodes, err := listNodes(ctx, tx, machineID)
	if err != nil {
		return err
	}
	state := protocol.EmptyDesiredState()
	for _, node := range nodes {
		private, err := security.OpenSecret(s.masterKey, node.privateKey, "node-key:"+node.NodeID)
		if err != nil {
			return err
		}
		desired := protocol.DesiredNode{
			ID: node.NodeID, Name: node.Name, PublicHost: node.PublicHost, PublicPort: node.PublicPort,
			ListenHost: node.ListenHost, ListenPort: node.ListenPort, Enabled: node.Enabled,
			Reality:     protocol.RealityConfig{ServerName: node.ServerName, Target: node.Target, PrivateKey: string(private), PublicKey: node.PublicKey, ShortID: node.ShortID},
			Credentials: []protocol.ProxyCredential{}, ExitMachineID: node.ExitMachineID,
		}
		desired.Credentials, err = s.nodeCredentialsTx(ctx, tx, node.NodeID)
		if err != nil {
			return err
		}
		if node.Development {
			uuid, err := s.readDevelopmentUUID(ctx, tx)
			if err != nil {
				return err
			}
			desired.Credentials = append(desired.Credentials, protocol.ProxyCredential{UserID: "development", UUID: uuid, Email: "development-" + node.NodeID, Development: true})
		}
		state.Nodes = append(state.Nodes, desired)
	}
	state.PeriodPolicies, err = machinePoliciesTx(ctx, tx, machineID, state)
	if err != nil {
		return err
	}
	state.Exit, state.TransitOutbounds, err = s.desiredTransitTx(ctx, tx, machineID, state)
	if err != nil {
		return err
	}
	state.QuotaRestrictions, err = s.quotaRestrictionsTx(ctx, tx, machineID, state, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := protocol.ValidateDesiredState(state); err != nil {
		return fmt.Errorf("validate machine snapshot: %w", err)
	}
	digest, err := protocol.DigestDesiredState(state)
	if err != nil {
		return err
	}
	var previous string
	if err := tx.QueryRowContext(ctx, "SELECT desired_digest FROM machines WHERE machine_id=?", machineID).Scan(&previous); err != nil {
		return err
	}
	if previous == digest {
		return nil
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	encrypted, err := security.SealSecret(s.masterKey, encoded, "machine-desired:"+machineID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE machines SET desired_revision = desired_revision + 1, desired_digest = ?, desired_snapshot = ?,
 pending_authority_epoch = NULL, pending_authority_nonce = NULL WHERE machine_id = ?`, digest, encrypted, machineID)
	if err != nil {
		return err
	}
	return nil
}

func (s *Store) GetMachineDesiredState(ctx context.Context, machineID string) (PanelAuthority, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PanelAuthority{}, err
	}
	defer tx.Rollback()
	machine, err := getMachineTx(ctx, tx, machineID)
	if err != nil {
		return PanelAuthority{}, err
	}
	authority, err := s.machineDesiredTx(ctx, tx, machine)
	if err != nil {
		return PanelAuthority{}, err
	}
	return authority, tx.Commit()
}

func (s *Store) machineDesiredTx(ctx context.Context, tx *sql.Tx, machine MachineRecord) (PanelAuthority, error) {
	authority, err := readPanelAuthority(ctx, tx)
	if err != nil {
		return PanelAuthority{}, err
	}
	authority.Revision, authority.Digest = machine.DesiredRevision, machine.DesiredDigest
	var encrypted []byte
	if err := tx.QueryRowContext(ctx, "SELECT desired_snapshot FROM machines WHERE machine_id = ?", machine.MachineID).Scan(&encrypted); err != nil {
		return PanelAuthority{}, err
	}
	if len(encrypted) != 0 {
		plain, err := security.OpenSecret(s.masterKey, encrypted, "machine-desired:"+machine.MachineID)
		if err != nil {
			return PanelAuthority{}, err
		}
		if err := json.Unmarshal(plain, &authority.Snapshot); err != nil {
			return PanelAuthority{}, err
		}
	}
	digest, err := protocol.DigestDesiredState(authority.Snapshot)
	if err != nil || digest != authority.Digest {
		return PanelAuthority{}, errors.New("machine snapshot digest is inconsistent")
	}
	return authority, nil
}
