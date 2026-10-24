package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
)

var ErrClosedReceipt = errors.New("无效的账本关闭凭据")

// This receipt proves a committed contiguous prefix, not the missing byte
// amounts. Agents retain it; only the Panel holding the master key can verify it.
type ClosedClaims struct {
	Version        int    `json:"version"`
	InstallationID string `json:"installation_id"`
	MachineID      string `json:"machine_id"`
	LedgerEpoch    string `json:"ledger_epoch"`
	ClosedThrough  uint64 `json:"closed_through,string"`
}
type ClosedReceipt struct {
	ClosedClaims
	MAC string `json:"mac"`
}

func (c ClosedClaims) Validate() error {
	if c.Version != 1 || !IsUUIDv4(c.InstallationID) || !IsUUIDv4(c.MachineID) || !IsUUIDv4(c.LedgerEpoch) || c.ClosedThrough == 0 || c.ClosedThrough > 1<<63-1 {
		return ErrClosedReceipt
	}
	return nil
}
func closedMAC(master []byte, claims ClosedClaims) ([]byte, error) {
	if len(master) != MasterKeySize || claims.Validate() != nil {
		return nil, ErrClosedReceipt
	}
	kdf := hmac.New(sha256.New, master)
	kdf.Write([]byte("panel-agent/closed-prefix-key/v1"))
	key := kdf.Sum(nil)
	defer clear(key)
	// Versioned JSON array of strings: fixed ordering, no map serialization,
	// ambiguous separators, machine precision or locale-dependent conversion.
	payload, _ := json.Marshal([]string{"panel-agent/closed-prefix/v1", strconv.Itoa(claims.Version), claims.InstallationID, claims.MachineID, claims.LedgerEpoch, strconv.FormatUint(claims.ClosedThrough, 10)})
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return mac.Sum(nil), nil
}
func SignClosedReceipt(master []byte, claims ClosedClaims) (ClosedReceipt, error) {
	mac, err := closedMAC(master, claims)
	if err != nil {
		return ClosedReceipt{}, err
	}
	return ClosedReceipt{claims, base64.RawURLEncoding.EncodeToString(mac)}, nil
}
func VerifyClosedReceipt(master []byte, receipt ClosedReceipt, installation, machine, epoch string) error {
	if receipt.InstallationID != installation || receipt.MachineID != machine || receipt.LedgerEpoch != epoch {
		return ErrClosedReceipt
	}
	want, err := closedMAC(master, receipt.ClosedClaims)
	if err != nil {
		return err
	}
	got, err := base64.RawURLEncoding.DecodeString(receipt.MAC)
	if err != nil || !hmac.Equal(want, got) {
		return ErrClosedReceipt
	}
	return nil
}
