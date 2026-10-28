package hysteria

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"runtime"
)

const SupportedVersion = "2.13.0"

// VerifyOfficialBinary pins the exact official release asset. A locally
// executable's version output alone cannot establish its origin.
func VerifyOfficialBinary(binary, architecture string) error {
	_, err := VerifiedVersion(binary, architecture)
	return err
}

func VerifiedVersion(binary, architecture string) (string, error) {
	assets := map[string]map[string]string{
		"amd64": {"907ba8c9693edb104b20582681fb7dc15639d5b64a9cbb616a7b539190a86691": "2.13.0", "8c7a68a906998b747a0db87586e364f995fbfddb95693ae6e2fdb68a6e920d3e": "2.12.3"},
		"arm64": {"a68a61a84452ca250ce0368202521965ca9cc9d801a404f1dc9008ac6cf677a7": "2.13.0", "c8dc653c3ba0a28d29a26b8fa52d2086f27c0927afddce95c09965e7174e78b0": "2.12.3"},
	}
	info, err := os.Lstat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<20 || assets[architecture] == nil {
		return "", errors.New("hysteria: unsupported or unsafe official executable")
	}
	if runtime.GOOS != "windows" && (info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0111 == 0) {
		return "", errors.New("hysteria: unsafe official executable permissions")
	}
	f, err := os.Open(binary)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	version := assets[architecture][hex.EncodeToString(hash.Sum(nil))]
	if version == "" {
		return "", errors.New("hysteria: official asset checksum mismatch")
	}
	return version, nil
}
