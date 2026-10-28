package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"panel-agent.local/panel-agent/internal/extensions"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

var (
	ErrExtensionExists     = errors.New("扩展版本已存在")
	ErrExtensionNotFound   = errors.New("扩展版本不存在")
	ErrExtensionReferenced = errors.New("扩展版本仍被节点引用")
	ErrExtensionDisabled   = errors.New("扩展版本已停用，不能新建依赖")
	ErrInvalidExtension    = errors.New("扩展配置无效")
)

type ExtensionRecord struct {
	ID          string
	Version     string
	Name        string
	Description string
	Enabled     bool
	Package     extensions.Package
}

type NodeExtensionInput struct {
	ID      string
	Version string
	Values  map[string]string
}

type nodeExtensionRecord struct {
	ID           string
	Version      string
	PublicConfig map[string]string
	SecretValues map[string]string
	Enabled      bool
	Package      extensions.Package
}

func (s *Store) InstallExtension(ctx context.Context, pkg extensions.Package, now time.Time) (ExtensionRecord, error) {
	canonical, err := extensions.CanonicalJSON(pkg)
	if err != nil {
		return ExtensionRecord{}, err
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `INSERT INTO protocol_extensions(extension_id,version,name,description,package_json,enabled,created_at,updated_at) VALUES(?,?,?,?,?,0,?,?)`, pkg.Manifest.ID, pkg.Manifest.Version, pkg.Manifest.Name, pkg.Manifest.Description, canonical, nowText, nowText)
	if err != nil {
		var count int
		if queryErr := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM protocol_extensions WHERE extension_id=? AND version=?", pkg.Manifest.ID, pkg.Manifest.Version).Scan(&count); queryErr == nil && count != 0 {
			return ExtensionRecord{}, ErrExtensionExists
		}
		return ExtensionRecord{}, fmt.Errorf("persist extension: %w", err)
	}
	return ExtensionRecord{ID: pkg.Manifest.ID, Version: pkg.Manifest.Version, Name: pkg.Manifest.Name, Description: pkg.Manifest.Description, Package: pkg}, nil
}

func (s *Store) GetExtension(ctx context.Context, id, version string) (ExtensionRecord, error) {
	var name, description string
	var packageJSON []byte
	var enabled bool
	err := s.db.QueryRowContext(ctx, "SELECT name,description,package_json,enabled FROM protocol_extensions WHERE extension_id=? AND version=?", id, version).Scan(&name, &description, &packageJSON, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ExtensionRecord{}, ErrExtensionNotFound
	}
	if err != nil {
		return ExtensionRecord{}, err
	}
	pkg, err := extensions.ParseJSON(packageJSON)
	if err != nil {
		return ExtensionRecord{}, err
	}
	return ExtensionRecord{ID: id, Version: version, Name: name, Description: description, Enabled: enabled, Package: pkg}, nil
}

func (s *Store) ListExtensions(ctx context.Context) ([]ExtensionRecord, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT extension_id,version,name,description,package_json,enabled FROM protocol_extensions ORDER BY extension_id,version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ExtensionRecord{}
	for rows.Next() {
		var item ExtensionRecord
		var packageJSON []byte
		if err := rows.Scan(&item.ID, &item.Version, &item.Name, &item.Description, &packageJSON, &item.Enabled); err != nil {
			return nil, err
		}
		item.Package, err = extensions.ParseJSON(packageJSON)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) SetExtensionEnabled(ctx context.Context, id, version string, enabled bool, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE protocol_extensions SET enabled=?,updated_at=? WHERE extension_id=? AND version=?", enabled, now.UTC().Format(time.RFC3339Nano), id, version)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrExtensionNotFound
	}
	if err := s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteExtension(ctx context.Context, id, version string) error {
	var references int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM node_extensions WHERE extension_id=? AND extension_version=?", id, version).Scan(&references); err != nil {
		return err
	}
	if references != 0 {
		return ErrExtensionReferenced
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM protocol_extensions WHERE extension_id=? AND version=?", id, version)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrExtensionNotFound
	}
	return nil
}

func (s *Store) SaveNodeWithExtension(ctx context.Context, machineID, nodeID string, input NodeInput, assignment *NodeExtensionInput, now time.Time) (NodeRecord, error) {
	input, err := normalizeNode(input)
	if err != nil {
		return NodeRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeRecord{}, err
	}
	defer tx.Rollback()
	node, err := s.saveNodeTx(ctx, tx, machineID, nodeID, input, now)
	if err != nil {
		return NodeRecord{}, err
	}
	if err := s.saveNodeExtensionTx(ctx, tx, node.MachineID, node.NodeID, assignment, now); err != nil {
		return NodeRecord{}, err
	}
	if err := s.refreshMachineDesiredTx(ctx, tx, machineID); err != nil {
		return NodeRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeRecord{}, err
	}
	return s.GetNode(ctx, machineID, node.NodeID)
}

func (s *Store) saveNodeExtensionTx(ctx context.Context, tx *sql.Tx, machineID, nodeID string, assignment *NodeExtensionInput, now time.Time) error {
	if assignment == nil || assignment.ID == "" {
		_, err := tx.ExecContext(ctx, "DELETE FROM node_extensions WHERE node_id=? AND machine_id=?", nodeID, machineID)
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE node_id=? AND machine_id=? AND deleted_at IS NULL", nodeID, machineID).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return ErrNodeNotFound
	}
	var enabled bool
	var packageJSON []byte
	if err := tx.QueryRowContext(ctx, "SELECT enabled,package_json FROM protocol_extensions WHERE extension_id=? AND version=?", assignment.ID, assignment.Version).Scan(&enabled, &packageJSON); errors.Is(err, sql.ErrNoRows) {
		return ErrExtensionNotFound
	} else if err != nil {
		return err
	}
	var current nodeExtensionRecord
	var currentJSON, currentSecrets []byte
	var currentEnabled bool
	err := tx.QueryRowContext(ctx, `SELECT ne.public_config,ne.encrypted_secrets,pe.enabled FROM node_extensions ne JOIN protocol_extensions pe ON pe.extension_id=ne.extension_id AND pe.version=ne.extension_version WHERE ne.node_id=?`, nodeID).Scan(&currentJSON, &currentSecrets, &currentEnabled)
	if err == nil {
		current.ID, current.Version = assignment.ID, assignment.Version
		_ = json.Unmarshal(currentJSON, &current.PublicConfig)
		plain, openErr := security.OpenSecret(s.masterKey, currentSecrets, "extension-secrets:"+nodeID)
		if openErr != nil {
			return openErr
		}
		if json.Unmarshal(plain, &current.SecretValues) != nil {
			return ErrInvalidExtension
		}
		current.Enabled = currentEnabled
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	values := map[string]string{}
	for key, value := range assignment.Values {
		values[key] = value
	}
	if current.ID == assignment.ID && current.Version == assignment.Version {
		for key, value := range current.PublicConfig {
			if values[key] == "" {
				values[key] = value
			}
		}
		for key, value := range current.SecretValues {
			if values[key] == "" {
				values[key] = value
			}
		}
	}
	pkg, err := extensions.ParseJSON(packageJSON)
	if err != nil {
		return err
	}
	compiled, err := extensions.CompileNode(pkg, values)
	if err != nil {
		return err
	}
	if !enabled && (current.ID != assignment.ID || current.Version != assignment.Version) {
		return ErrExtensionDisabled
	}
	publicJSON, err := json.Marshal(map[string]string{"sni": compiled.SNI})
	if err != nil {
		return err
	}
	secretJSON, err := json.Marshal(map[string]string{"certificate_pem": compiled.CertificatePEM, "private_key_pem": compiled.PrivateKeyPEM})
	if err != nil {
		return err
	}
	encrypted, err := security.SealSecret(s.masterKey, secretJSON, "extension-secrets:"+nodeID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO node_extensions(node_id,machine_id,extension_id,extension_version,public_config,encrypted_secrets,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET machine_id=excluded.machine_id,extension_id=excluded.extension_id,extension_version=excluded.extension_version,public_config=excluded.public_config,encrypted_secrets=excluded.encrypted_secrets,updated_at=excluded.updated_at`, nodeID, machineID, assignment.ID, assignment.Version, publicJSON, encrypted, now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetNode(ctx context.Context, machineID, nodeID string) (NodeRecord, error) {
	nodes, err := s.ListNodes(ctx, machineID)
	if err != nil {
		return NodeRecord{}, err
	}
	for _, node := range nodes {
		if node.NodeID == nodeID {
			return node, nil
		}
	}
	return NodeRecord{}, ErrNodeNotFound
}

func (s *Store) populateNodeExtension(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, node *NodeRecord) error {
	node.ExtensionEnabled = false
	var publicJSON, packageJSON []byte
	err := query.QueryRowContext(ctx, `SELECT ne.extension_id,ne.extension_version,ne.public_config,pe.enabled,pe.package_json FROM node_extensions ne JOIN protocol_extensions pe ON pe.extension_id=ne.extension_id AND pe.version=ne.extension_version WHERE ne.node_id=?`, node.NodeID).Scan(&node.ExtensionID, &node.ExtensionVersion, &publicJSON, &node.ExtensionEnabled, &packageJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var publicConfig map[string]string
	if err := json.Unmarshal(publicJSON, &publicConfig); err != nil {
		return ErrInvalidExtension
	}
	pkg, err := extensions.ParseJSON(packageJSON)
	if err != nil {
		return err
	}
	node.ExtensionKind = extensions.RendererKind(pkg)
	node.ExtensionSNI = publicConfig["sni"]
	return nil
}

func (s *Store) nodeExtensionTx(ctx context.Context, tx *sql.Tx, nodeID string) (*protocol.ExtensionNode, bool, error) {
	var id, version string
	var publicJSON, encrypted []byte
	var enabled bool
	err := tx.QueryRowContext(ctx, `SELECT ne.extension_id,ne.extension_version,ne.public_config,ne.encrypted_secrets,pe.enabled FROM node_extensions ne JOIN protocol_extensions pe ON pe.extension_id=ne.extension_id AND pe.version=ne.extension_version WHERE ne.node_id=?`, nodeID).Scan(&id, &version, &publicJSON, &encrypted, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	var publicConfig, secretValues map[string]string
	if json.Unmarshal(publicJSON, &publicConfig) != nil {
		return nil, false, ErrInvalidExtension
	}
	plain, err := security.OpenSecret(s.masterKey, encrypted, "extension-secrets:"+nodeID)
	if err != nil || json.Unmarshal(plain, &secretValues) != nil {
		return nil, false, ErrInvalidExtension
	}
	var packageJSON []byte
	if err := tx.QueryRowContext(ctx, "SELECT package_json FROM protocol_extensions WHERE extension_id=? AND version=?", id, version).Scan(&packageJSON); err != nil {
		return nil, false, err
	}
	pkg, err := extensions.ParseJSON(packageJSON)
	if err != nil {
		return nil, false, err
	}
	values := map[string]string{}
	for key, value := range publicConfig {
		values[key] = value
	}
	for key, value := range secretValues {
		values[key] = value
	}
	compiled, err := extensions.CompileNode(pkg, values)
	if err != nil {
		return nil, false, err
	}
	return &protocol.ExtensionNode{CoreMinVersion: compiled.CoreMinVersion, ID: compiled.ID, Version: compiled.Version, Kind: compiled.Kind, SNI: compiled.SNI, CertificatePEM: compiled.CertificatePEM, PrivateKeyPEM: compiled.PrivateKeyPEM}, enabled, nil
}
