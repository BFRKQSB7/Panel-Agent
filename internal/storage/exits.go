package storage

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

var (
	ErrInvalidExit  = errors.New("Exit 配置无效")
	ErrExitNotFound = errors.New("Exit 不存在")
	ErrExitInUse    = errors.New("Exit 仍被 Node 使用")
)

type ExitInput struct {
	PublicHost string `json:"public_host"`
	PublicPort int    `json:"public_port"`
	ListenHost string `json:"listen_host"`
	ListenPort int    `json:"listen_port"`
	ServerName string `json:"server_name"`
	Target     string `json:"target"`
}

type ExitRecord struct {
	MachineID string `json:"machine_id"`
	ExitInput
	PublicKey  string `json:"public_key"`
	ShortID    string `json:"short_id"`
	privateKey []byte
}

type ExitOption struct {
	MachineID   string `json:"machine_id"`
	DisplayName string `json:"display_name"`
	PublicHost  string `json:"public_host"`
	PublicPort  int    `json:"public_port"`
	Ready       bool   `json:"ready"`
}

func normalizeExit(input ExitInput) (ExitInput, error) {
	var err error
	input.PublicHost, err = protocol.NormalizeHost(input.PublicHost)
	if err != nil || input.PublicPort < 1 || input.PublicPort > 65535 {
		return input, ErrInvalidExit
	}
	if net.ParseIP(strings.TrimSpace(input.ListenHost)) == nil || input.ListenPort < 1 || input.ListenPort > 65535 || input.ListenPort == protocol.CoreStatsPort {
		return input, ErrInvalidExit
	}
	input.ListenHost = net.ParseIP(strings.TrimSpace(input.ListenHost)).String()
	input.ServerName, err = protocol.NormalizeHost(input.ServerName)
	if err != nil || net.ParseIP(input.ServerName) != nil {
		return input, ErrInvalidExit
	}
	if strings.TrimSpace(input.Target) == "" {
		input.Target = net.JoinHostPort(input.ServerName, "443")
	}
	host, portText, err := net.SplitHostPort(strings.TrimSpace(input.Target))
	if err != nil {
		return input, ErrInvalidExit
	}
	host, err = protocol.NormalizeHost(host)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || port < 1 || port > 65535 {
		return input, ErrInvalidExit
	}
	input.Target = net.JoinHostPort(host, strconv.Itoa(port))
	return input, nil
}

const exitSelect = `SELECT machine_id, public_host, public_port, listen_host, listen_port,
 server_name, reality_target, private_key, public_key, short_id FROM exits`

func scanExit(scanner rowScanner) (ExitRecord, error) {
	var exit ExitRecord
	err := scanner.Scan(&exit.MachineID, &exit.PublicHost, &exit.PublicPort, &exit.ListenHost, &exit.ListenPort,
		&exit.ServerName, &exit.Target, &exit.privateKey, &exit.PublicKey, &exit.ShortID)
	return exit, err
}

func exitKeyOwner(machineID string) string { return "exit-key:" + machineID }

func (s *Store) GetExit(ctx context.Context, machineID string) (ExitRecord, error) {
	exit, err := scanExit(s.db.QueryRowContext(ctx, exitSelect+" WHERE machine_id=?", machineID))
	if errors.Is(err, sql.ErrNoRows) {
		return ExitRecord{}, ErrExitNotFound
	}
	return exit, err
}

func (s *Store) ListExitOptions(ctx context.Context, excludeMachineID string) ([]ExitOption, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT e.machine_id,m.display_name,e.public_host,e.public_port
		FROM exits e JOIN machines m ON m.machine_id=e.machine_id
		WHERE e.machine_id != ? ORDER BY m.display_name,m.machine_id`, excludeMachineID)
	if err != nil {
		return nil, err
	}
	options := []ExitOption{}
	for rows.Next() {
		var option ExitOption
		if err := rows.Scan(&option.MachineID, &option.DisplayName, &option.PublicHost, &option.PublicPort); err != nil {
			rows.Close()
			return nil, err
		}
		option.Ready, err = s.exitReadyTx(ctx, tx, option.MachineID)
		if err != nil {
			rows.Close()
			return nil, err
		}
		options = append(options, option)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return options, tx.Commit()
}

func (s *Store) SaveExit(ctx context.Context, machineID string, input ExitInput, now time.Time) (ExitRecord, error) {
	if !security.IsUUIDv4(machineID) {
		return ExitRecord{}, ErrInvalidExit
	}
	input, err := normalizeExit(input)
	if err != nil {
		return ExitRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExitRecord{}, err
	}
	defer tx.Rollback()
	machine, err := getMachineTx(ctx, tx, machineID)
	if err != nil {
		return ExitRecord{}, err
	}
	if machine.Revoked {
		return ExitRecord{}, ErrMachineAuthentication
	}
	var conflicts int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE machine_id=? AND listen_port=? AND deleted_at IS NULL", machineID, input.ListenPort).Scan(&conflicts); err != nil {
		return ExitRecord{}, err
	}
	if conflicts != 0 {
		return ExitRecord{}, ErrNodePortConflict
	}
	exit, readErr := scanExit(tx.QueryRowContext(ctx, exitSelect+" WHERE machine_id=?", machineID))
	creating := errors.Is(readErr, sql.ErrNoRows)
	if readErr != nil && !creating {
		return ExitRecord{}, readErr
	}
	if creating {
		key, keyErr := ecdh.X25519().GenerateKey(rand.Reader)
		if keyErr != nil {
			return ExitRecord{}, keyErr
		}
		exit.MachineID = machineID
		exit.privateKey = key.Bytes()
		exit.PublicKey = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
		shortID := make([]byte, 8)
		if _, err := rand.Read(shortID); err != nil {
			return ExitRecord{}, err
		}
		exit.ShortID = hex.EncodeToString(shortID)
	}
	exit.ExitInput, exit.MachineID = input, machineID
	nowText := now.UTC().Format(time.RFC3339Nano)
	if creating {
		sealed, err := security.SealSecret(s.masterKey, []byte(base64.RawURLEncoding.EncodeToString(exit.privateKey)), exitKeyOwner(machineID))
		if err != nil {
			return ExitRecord{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO exits(machine_id,public_host,public_port,listen_host,listen_port,server_name,reality_target,private_key,public_key,short_id,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, machineID, input.PublicHost, input.PublicPort, input.ListenHost, input.ListenPort, input.ServerName, input.Target, sealed, exit.PublicKey, exit.ShortID, nowText, nowText)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE exits SET public_host=?,public_port=?,listen_host=?,listen_port=?,server_name=?,reality_target=?,updated_at=? WHERE machine_id=?`, input.PublicHost, input.PublicPort, input.ListenHost, input.ListenPort, input.ServerName, input.Target, nowText, machineID)
	}
	if err != nil {
		return ExitRecord{}, err
	}
	if err := s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return ExitRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExitRecord{}, err
	}
	exit.privateKey = nil
	return exit, nil
}

func (s *Store) DeleteExit(ctx context.Context, machineID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := scanExit(tx.QueryRowContext(ctx, exitSelect+" WHERE machine_id=?", machineID)); errors.Is(err, sql.ErrNoRows) {
		return ErrExitNotFound
	} else if err != nil {
		return err
	}
	var references int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE exit_machine_id=? AND deleted_at IS NULL", machineID).Scan(&references); err != nil {
		return err
	}
	if references != 0 {
		return ErrExitInUse
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM transit_credentials WHERE exit_machine_id=?", machineID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM exits WHERE machine_id=?", machineID); err != nil {
		return err
	}
	if err := s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RotateExitTransit(ctx context.Context, exitMachineID string, now time.Time) error {
	if !security.IsUUIDv4(exitMachineID) {
		return ErrInvalidExit
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := scanExit(tx.QueryRowContext(ctx, exitSelect+" WHERE machine_id=?", exitMachineID)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrExitNotFound
		}
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT entry_machine_id,generation FROM transit_credentials WHERE exit_machine_id=? ORDER BY entry_machine_id", exitMachineID)
	if err != nil {
		return err
	}
	type pair struct {
		entry      string
		generation int64
	}
	pairs := []pair{}
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.entry, &p.generation); err != nil {
			rows.Close()
			return err
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, p := range pairs {
		if p.generation >= 1<<63-1 {
			return ErrInvalidExit
		}
		uuid, err := security.NewUUID()
		if err != nil {
			return err
		}
		sealed, err := security.SealSecret(s.masterKey, []byte(uuid), transitCredentialOwner(p.entry, exitMachineID))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE transit_credentials SET encrypted_uuid=?,generation=generation+1,updated_at=? WHERE entry_machine_id=? AND exit_machine_id=?", sealed, now.UTC().Format(time.RFC3339Nano), p.entry, exitMachineID); err != nil {
			return err
		}
	}
	if err := s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func transitCredentialOwner(entryMachineID, exitMachineID string) string {
	return "transit-credential:" + entryMachineID + ":" + exitMachineID
}

func transitEmail(entryMachineID, exitMachineID string) string {
	return "transit-" + entryMachineID + "-" + exitMachineID
}

func (s *Store) syncTransitCredentialsTx(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT machine_id,exit_machine_id FROM nodes
		WHERE deleted_at IS NULL AND exit_machine_id IS NOT NULL ORDER BY machine_id,exit_machine_id`)
	if err != nil {
		return nil, err
	}
	type pair struct{ entry, exit string }
	pairs := []pair{}
	needed := map[pair]bool{}
	changed := map[string]bool{}
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.entry, &p.exit); err != nil {
			rows.Close()
			return nil, err
		}
		if p.entry == p.exit || !security.IsUUIDv4(p.entry) || !security.IsUUIDv4(p.exit) {
			rows.Close()
			return nil, ErrInvalidExit
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM exits WHERE machine_id=?", p.exit).Scan(&count); err != nil {
			rows.Close()
			return nil, err
		}
		if count != 1 {
			rows.Close()
			return nil, ErrExitNotFound
		}
		if !needed[p] {
			needed[p] = true
			pairs = append(pairs, p)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, p := range pairs {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM transit_credentials WHERE entry_machine_id=? AND exit_machine_id=?", p.entry, p.exit).Scan(&count); err != nil {
			return nil, err
		}
		if count != 0 {
			continue
		}
		uuid, err := security.NewUUID()
		if err != nil {
			return nil, err
		}
		sealed, err := security.SealSecret(s.masterKey, []byte(uuid), transitCredentialOwner(p.entry, p.exit))
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO transit_credentials(entry_machine_id,exit_machine_id,encrypted_uuid,generation,updated_at) VALUES(?,?,?,1,?)", p.entry, p.exit, sealed, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		changed[p.exit] = true
	}
	staleRows, err := tx.QueryContext(ctx, `SELECT DISTINCT exit_machine_id FROM transit_credentials
		WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.machine_id=transit_credentials.entry_machine_id AND n.exit_machine_id=transit_credentials.exit_machine_id AND n.deleted_at IS NULL)`)
	if err != nil {
		return nil, err
	}
	for staleRows.Next() {
		var exitMachineID string
		if err := staleRows.Scan(&exitMachineID); err != nil {
			staleRows.Close()
			return nil, err
		}
		changed[exitMachineID] = true
	}
	if err := staleRows.Err(); err != nil {
		staleRows.Close()
		return nil, err
	}
	staleRows.Close()
	if _, err = tx.ExecContext(ctx, `DELETE FROM transit_credentials
		WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.machine_id=transit_credentials.entry_machine_id AND n.exit_machine_id=transit_credentials.exit_machine_id AND n.deleted_at IS NULL)`); err != nil {
		return nil, err
	}
	exitMachines := make([]string, 0, len(changed))
	for exitMachineID := range changed {
		exitMachines = append(exitMachines, exitMachineID)
	}
	sort.Strings(exitMachines)
	return exitMachines, nil
}

func (s *Store) refreshDependentMachineDesiredTx(ctx context.Context, tx *sql.Tx, exitMachineID string) error {
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT machine_id FROM nodes WHERE exit_machine_id=? AND deleted_at IS NULL ORDER BY machine_id", exitMachineID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if err := s.refreshMachineDesiredTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readTransitUUIDTx(ctx context.Context, tx *sql.Tx, entryMachineID, exitMachineID string) (string, error) {
	var encrypted []byte
	if err := tx.QueryRowContext(ctx, "SELECT encrypted_uuid FROM transit_credentials WHERE entry_machine_id=? AND exit_machine_id=?", entryMachineID, exitMachineID).Scan(&encrypted); err != nil {
		return "", err
	}
	plain, err := security.OpenSecret(s.masterKey, encrypted, transitCredentialOwner(entryMachineID, exitMachineID))
	if err != nil || !security.IsUUIDv4(string(plain)) {
		return "", ErrInvalidExit
	}
	return string(plain), nil
}

func (s *Store) desiredTransitTx(ctx context.Context, tx *sql.Tx, machineID string, state protocol.DesiredState) (*protocol.DesiredExit, []protocol.TransitOutbound, error) {
	var desiredExit *protocol.DesiredExit
	exit, err := scanExit(tx.QueryRowContext(ctx, exitSelect+" WHERE machine_id=?", machineID))
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	} else if err != nil {
		return nil, nil, err
	} else {
		private, err := security.OpenSecret(s.masterKey, exit.privateKey, exitKeyOwner(machineID))
		if err != nil {
			return nil, nil, err
		}
		rows, err := tx.QueryContext(ctx, "SELECT entry_machine_id,encrypted_uuid FROM transit_credentials WHERE exit_machine_id=? ORDER BY entry_machine_id", machineID)
		if err != nil {
			return nil, nil, err
		}
		credentials := []protocol.TransitCredential{}
		for rows.Next() {
			var entry string
			var encrypted []byte
			if err := rows.Scan(&entry, &encrypted); err != nil {
				rows.Close()
				return nil, nil, err
			}
			plain, err := security.OpenSecret(s.masterKey, encrypted, transitCredentialOwner(entry, machineID))
			if err != nil || !security.IsUUIDv4(string(plain)) {
				rows.Close()
				return nil, nil, ErrInvalidExit
			}
			credentials = append(credentials, protocol.TransitCredential{EntryMachineID: entry, UUID: string(plain), Email: transitEmail(entry, machineID)})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
		desiredExit = &protocol.DesiredExit{MachineID: exit.MachineID, PublicHost: exit.PublicHost, PublicPort: exit.PublicPort, ListenHost: exit.ListenHost, ListenPort: exit.ListenPort,
			Reality: protocol.RealityConfig{ServerName: exit.ServerName, Target: exit.Target, PrivateKey: string(private), PublicKey: exit.PublicKey, ShortID: exit.ShortID}, Credentials: credentials}
	}
	outbounds := []protocol.TransitOutbound{}
	seen := map[string]bool{}
	for i := range state.Nodes {
		node := &state.Nodes[i]
		if node.ExitMachineID == "" || !node.Enabled {
			continue
		}
		ready, err := s.exitReadyTx(ctx, tx, node.ExitMachineID)
		if err != nil {
			return nil, nil, err
		}
		if !ready {
			node.Enabled = false
			continue
		}
		target, err := scanExit(tx.QueryRowContext(ctx, exitSelect+" WHERE machine_id=?", node.ExitMachineID))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				node.Enabled = false
				continue
			}
			return nil, nil, err
		}
		uuid, err := s.readTransitUUIDTx(ctx, tx, machineID, node.ExitMachineID)
		if err != nil {
			return nil, nil, err
		}
		if !seen[node.ExitMachineID] {
			seen[node.ExitMachineID] = true
			outbounds = append(outbounds, protocol.TransitOutbound{ExitMachineID: node.ExitMachineID, PublicHost: target.PublicHost, PublicPort: target.PublicPort, UUID: uuid, ServerName: target.ServerName, PublicKey: target.PublicKey, ShortID: target.ShortID})
		}
	}
	sort.Slice(outbounds, func(i, j int) bool { return outbounds[i].ExitMachineID < outbounds[j].ExitMachineID })
	return desiredExit, outbounds, nil
}

func (s *Store) exitReadyTx(ctx context.Context, tx *sql.Tx, machineID string) (bool, error) {
	machine, err := getMachineTx(ctx, tx, machineID)
	if err != nil {
		return false, err
	}
	if machine.Revoked || machine.ApplyError != "" || machine.AppliedRevision != machine.DesiredRevision || machine.AppliedDigest != machine.DesiredDigest || machine.AppliedAuthorityEpoch != machine.AuthorityEpoch {
		return false, nil
	}
	authority, err := s.machineDesiredTx(ctx, tx, machine)
	if err != nil {
		return false, err
	}
	return authority.Snapshot.Exit != nil && authority.Snapshot.Exit.MachineID == machineID && !authority.Snapshot.QuotaRestrictions.MachineStop, nil
}
