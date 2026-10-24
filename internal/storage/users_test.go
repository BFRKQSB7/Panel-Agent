package storage

import (
	"bytes"
	"context"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

func TestGroupAuthorizationCredentialsAndTombstones(t *testing.T) {
	s := openMachineTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	join, candidate, registration := testSecrets(t)
	if err := s.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, _, err := s.RegisterMachine(ctx, testRegistration(join, candidate, registration), now)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []NodeRecord{}
	for i := 0; i < 3; i++ {
		node, err := s.SaveNode(ctx, machine.MachineID, "", NodeInput{Name: "节点", PublicHost: "node.example.test", PublicPort: 45001 + i, ListenHost: "127.0.0.1", ListenPort: 18443 + i, Enabled: true, ServerName: "www.cloudflare.com"}, now)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	normal, err := s.SaveGroup(ctx, "node", "", GroupInput{Name: "普通节点", NodeIDs: []string{nodes[0].NodeID, nodes[1].NodeID, nodes[1].NodeID}}, now)
	if err != nil || len(normal.NodeIDs) != 2 {
		t.Fatal("node group deduplication", err)
	}
	overlap, err := s.SaveGroup(ctx, "node", "", GroupInput{Name: "重复路径", NodeIDs: []string{nodes[1].NodeID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	testGroup, err := s.SaveGroup(ctx, "node", "", GroupInput{Name: "实验节点", NodeIDs: []string{nodes[2].NodeID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := s.SaveGroup(ctx, "user", "", GroupInput{Name: "普通用户", NodeGroupIDs: []string{normal.GroupID, overlap.GroupID, normal.GroupID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	experimental, err := s.SaveGroup(ctx, "user", "", GroupInput{Name: "测试用户", NodeGroupIDs: []string{testGroup.GroupID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	zero := quantity.Bytes(0)
	alice, err := s.SaveUser(ctx, "", UserInput{Name: "Alice", UserGroupID: ordinary.GroupID, Enabled: true, QuotaBytes: &zero}, now)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.SaveUser(ctx, "", UserInput{Name: "Bob", UserGroupID: experimental.GroupID, Enabled: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	if alice.QuotaBytes == nil || *alice.QuotaBytes != 0 || bob.QuotaBytes != nil {
		t.Fatal("zero and unlimited quotas were conflated")
	}
	var encrypted []byte
	if err := s.db.QueryRowContext(ctx, "SELECT encrypted_secret FROM proxy_credentials WHERE user_id=?", alice.UserID).Scan(&encrypted); err != nil || bytes.Contains(encrypted, []byte(alice.CredentialUUID)) {
		t.Fatal("formal credential is not encrypted", err)
	}
	accessA, err := s.GetUserAccess(ctx, alice.UserID, now)
	if err != nil || len(accessA.Nodes) != 2 {
		t.Fatal("normal user node visibility", err)
	}
	accessB, err := s.GetUserAccess(ctx, bob.UserID, now)
	if err != nil || len(accessB.Nodes) != 1 || accessB.Nodes[0].NodeID != nodes[2].NodeID {
		t.Fatal("experimental user node visibility", err)
	}
	state, err := s.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	emails := map[string]string{}
	for _, node := range state.Snapshot.Nodes {
		if len(node.Credentials) != 1 {
			t.Fatal("group paths duplicated or omitted credentials")
		}
		credential := node.Credentials[0]
		if credential.Development {
			t.Fatal("formal snapshot contains implicit development credential")
		}
		if node.ID == nodes[2].NodeID {
			if credential.UserID != bob.UserID || credential.UUID != bob.CredentialUUID {
				t.Fatal("test Node gained normal credential")
			}
		} else {
			if credential.UserID != alice.UserID || credential.UUID != alice.CredentialUUID {
				t.Fatal("normal Nodes do not share the formal credential")
			}
			emails[node.ID] = credential.Email
		}
	}
	rotated, err := s.RotateUserCredential(ctx, alice.UserID, now)
	if err != nil || rotated.CredentialUUID == alice.CredentialUUID || rotated.CredentialGeneration != 2 {
		t.Fatal("credential rotation", err)
	}
	state, err = s.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range state.Snapshot.Nodes {
		if email, ok := emails[node.ID]; ok {
			if node.Credentials[0].UUID != rotated.CredentialUUID || node.Credentials[0].Email != email {
				t.Fatal("rotation rewrote historical statistics ownership")
			}
		}
	}
	newNode, err := s.SaveNode(ctx, machine.MachineID, "", NodeInput{Name: "新增节点", PublicHost: "node.example.test", PublicPort: 45004, ListenHost: "127.0.0.1", ListenPort: 18446, Enabled: true, ServerName: "www.cloudflare.com"}, now)
	if err != nil {
		t.Fatal(err)
	}
	normal.NodeIDs = append(normal.NodeIDs, newNode.NodeID)
	if _, err := s.SaveGroup(ctx, "node", normal.GroupID, normal.GroupInput, now); err != nil {
		t.Fatal(err)
	}
	accessA, err = s.GetUserAccess(ctx, alice.UserID, now)
	if err != nil || len(accessA.Nodes) != 3 {
		t.Fatal("new Node did not automatically inherit authorization", err)
	}
	if _, err := s.ChangeUsers(ctx, UserBatchInput{Operation: "move", UserIDs: []string{alice.UserID, "missing-user"}, UserGroupID: experimental.GroupID}, now); err != nil {
		t.Fatal(err)
	}
	accessA, err = s.GetUserAccess(ctx, alice.UserID, now)
	if err != nil || len(accessA.Nodes) != 1 || accessA.Nodes[0].NodeID != nodes[2].NodeID {
		t.Fatal("group move did not change actual authorization", err)
	}
	input := alice.UserInput
	input.Enabled = false
	input.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	disabled, err := s.SaveUser(ctx, alice.UserID, input, now)
	if err != nil || len(disabled.UnavailableReasons(now)) != 2 {
		t.Fatal("disable causes overwrote each other", err)
	}
	accessA, err = s.GetUserAccess(ctx, alice.UserID, now)
	if err != nil || len(accessA.Nodes) != 0 {
		t.Fatal("disabled/expired user remains visible", err)
	}
	batch, err := s.CreateUsers(ctx, []UserInput{{Name: "Batch A", UserGroupID: ordinary.GroupID, Enabled: true}, {Name: "Bob", UserGroupID: ordinary.GroupID, Enabled: true}, {Name: "Batch B", UserGroupID: ordinary.GroupID, Enabled: true}}, now)
	if err != nil || batch[0].Error != "" || batch[1].Error != "name_conflict" || batch[2].UserID == "" {
		t.Fatal("batch did not preserve visible partial success", err)
	}
	if err := s.DeleteUser(ctx, bob.UserID, now); err != nil {
		t.Fatal(err)
	}
	recreated, err := s.SaveUser(ctx, "", bob.UserInput, now)
	if err != nil || recreated.UserID == bob.UserID || recreated.CredentialUUID == bob.CredentialUUID {
		t.Fatal("deleted identity was reused", err)
	}
	var tombstones int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxy_users WHERE user_id=? AND deleted_at IS NOT NULL", bob.UserID).Scan(&tombstones); err != nil || tombstones != 1 {
		t.Fatal("late-traffic identity was discarded", err)
	}
	if err := s.DeleteGroup(ctx, "user", experimental.GroupID, now); err != nil {
		t.Fatal(err)
	}
	accessB, err = s.GetUserAccess(ctx, recreated.UserID, now)
	if err != nil || len(accessB.Nodes) != 0 || !accessB.User.GroupDeleted {
		t.Fatal("deleted group continued granting access", err)
	}
	state, err = s.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil || len(state.Snapshot.Nodes) != 4 {
		t.Fatal("authorization changes deleted Node definitions", err)
	}
	if err := protocol.ValidateDesiredState(state.Snapshot); err != nil {
		t.Fatal(err)
	}
}
