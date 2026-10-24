package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractCoreBinaryAndControlledReleaseURL(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, err := writer.Create("xray")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("candidate")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := extractCoreBinary(archive.Bytes())
	if err != nil || string(contents) != "candidate" {
		t.Fatalf("extracted candidate = %q, %v", contents, err)
	}
	url, err := officialXrayReleaseURL("26.10.22", "amd64")
	if err != nil || url != officialXrayReleaseBaseURL+"26.10.22/Xray-linux-64.zip" {
		t.Fatalf("controlled URL = %q, %v", url, err)
	}
	if _, err := officialXrayReleaseURL("26.10.22", "386"); err == nil {
		t.Fatal("unsupported architecture was accepted")
	}

	archive.Reset()
	writer = zip.NewWriter(&archive)
	if _, err := writer.Create("unexpected.txt"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractCoreBinary(archive.Bytes()); err == nil {
		t.Fatal("uncontrolled ZIP entry was accepted")
	}
}

func TestExtractConfiguredOfficialPackage(t *testing.T) {
	packagePath := os.Getenv("PANEL_AGENT_TEST_XRAY_PACKAGE")
	if packagePath == "" {
		t.Skip("set PANEL_AGENT_TEST_XRAY_PACKAGE for an actual official release archive")
	}
	contents, err := os.ReadFile(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := extractCoreBinary(contents)
	if err != nil || len(candidate) == 0 {
		t.Fatalf("official release extraction failed: %v", err)
	}
}

func TestRecoverPersistedUpgradeKeepsOriginalBeforeSwap(t *testing.T) {
	dir := t.TempDir()
	coreDir := filepath.Join(dir, "core")
	upgradeDir := filepath.Join(coreDir, coreUpgradeDirName, "11111111-1111-4111-8111-111111111111")
	if err := os.MkdirAll(upgradeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "xray")
	candidate := filepath.Join(upgradeDir, "xray")
	if err := os.WriteFile(binary, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := coreUpgradeJournal{Version: 1, IntentID: "11111111-1111-4111-8111-111111111111", Phase: "prepared", BinaryPath: binary, CandidatePath: candidate, BackupPath: filepath.Join(coreDir, coreUpgradeDirName, "11111111-1111-4111-8111-111111111111.old"), CurrentVersion: "26.10.21", TargetVersion: "26.10.22"}
	encoded, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coreDir, coreUpgradeJournalName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverPersistedUpgrade(coreDir, binary); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(binary)
	if err != nil || string(original) != "old" {
		t.Fatalf("original binary after recovery = %q, %v", original, err)
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("candidate remained after prepared recovery: %v", err)
	}
}

func TestRecoverPersistedUpgradeFailsClosedWhenSwappedBackupIsMissing(t *testing.T) {
	dir := t.TempDir()
	coreDir := filepath.Join(dir, "core")
	upgradeDir := filepath.Join(coreDir, coreUpgradeDirName, "11111111-1111-4111-8111-111111111111")
	if err := os.MkdirAll(upgradeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "xray")
	if err := os.WriteFile(binary, []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := coreUpgradeJournal{Version: 1, IntentID: "11111111-1111-4111-8111-111111111111", Phase: "swapped", BinaryPath: binary, CandidatePath: filepath.Join(upgradeDir, "xray"), BackupPath: filepath.Join(coreDir, coreUpgradeDirName, "11111111-1111-4111-8111-111111111111.old"), CurrentVersion: "26.10.21", TargetVersion: "26.10.22"}
	encoded, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coreDir, coreUpgradeJournalName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverPersistedUpgrade(coreDir, binary); err == nil {
		t.Fatal("swapped upgrade without rollback binary was accepted")
	}
}

func TestExternalCoreOwnershipIsNotReplaceable(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "xray")
	if err := os.WriteFile(binary, []byte("core"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ownership.json"), []byte(`{"core":{"owner":"external","path":"/srv/xray"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	owned, err := readCoreOwnership(dir, binary)
	if err != nil || owned {
		t.Fatalf("external Core ownership = (%v, %v)", owned, err)
	}
	manager := &CoreManager{}
	manager.coreOwned = owned
	if _, err := manager.UpgradeBinary(context.Background(), "11111111-1111-4111-8111-111111111111", "26.10.22", "", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("external Core upgrade was not rejected")
	}
}
