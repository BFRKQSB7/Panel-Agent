package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrWeakCapability = errors.New("自定义 Token 强度不足或格式无效")

type CapabilitySpec struct {
	Prefix string `json:"prefix,omitempty"`
	Value  string `json:"value,omitempty"`
}

func CapabilitySyntax(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, c := range []byte(value) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Structural checks cannot prove that a human-supplied value came from a CSPRNG.
// Default generation always uses 256 random bits; custom values must also pass
// these diversity and obvious-pattern checks, documented in the form.
func NewCapability(spec CapabilitySpec) (string, error) {
	if spec.Value != "" {
		value := spec.Value
		if spec.Prefix != "" || !CapabilitySyntax(value) {
			return "", ErrWeakCapability
		}
		seen := map[byte]bool{}
		lower, upper, digits := false, false, false
		for _, c := range []byte(value) {
			seen[c] = true
			lower = lower || c >= 'a' && c <= 'z'
			upper = upper || c >= 'A' && c <= 'Z'
			digits = digits || c >= '0' && c <= '9'
		}
		_, hexErr := hex.DecodeString(value)
		hexValue := hexErr == nil && len(value) >= 64 && len(seen) >= 12
		if !hexValue && !(len(seen) >= 20 && lower && upper && digits) {
			return "", ErrWeakCapability
		}
		for period := 1; period <= 16; period++ {
			repeated := true
			for i := period; i < len(value); i++ {
				if value[i] != value[i%period] {
					repeated = false
					break
				}
			}
			if repeated {
				return "", ErrWeakCapability
			}
		}
		ascending, descending := 1, 1
		for i := 1; i < len(value); i++ {
			if value[i] == value[i-1]+1 {
				ascending++
			} else {
				ascending = 1
			}
			if value[i]+1 == value[i-1] {
				descending++
			} else {
				descending = 1
			}
			if ascending >= 8 || descending >= 8 {
				return "", ErrWeakCapability
			}
		}
		return value, nil
	}
	if len(spec.Prefix) > 16 {
		return "", ErrWeakCapability
	}
	for _, c := range []byte(spec.Prefix) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", ErrWeakCapability
		}
	}
	value, err := NewToken(32)
	if err != nil {
		return "", err
	}
	if spec.Prefix != "" {
		value = spec.Prefix + "_" + value
	}
	return value, nil
}

type ActionSigner struct {
	mu  sync.RWMutex
	key []byte
}

func NewActionSigner(master []byte) (*ActionSigner, error) {
	if len(master) != MasterKeySize {
		return nil, errors.New("invalid action signing key")
	}
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte("panel-agent/action-signing/v1"))
	return &ActionSigner{key: mac.Sum(nil)}, nil
}

func (s *ActionSigner) Close() { s.mu.Lock(); defer s.mu.Unlock(); clear(s.key); s.key = nil }

func (s *ActionSigner) signature(payload string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.key) != 32 {
		return nil, errors.New("action signer is closed")
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil), nil
}

func (s *ActionSigner) Issue(purpose, subject string, now time.Time) (string, error) {
	if strings.ContainsAny(purpose+subject, "\n\r") || len(purpose)+len(subject) > 256 {
		return "", errors.New("invalid action scope")
	}
	id, err := NewUUID()
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte("1\n" + purpose + "\n" + subject + "\n" + strconv.FormatInt(now.Add(15*time.Minute).Unix(), 10) + "\n" + id))
	signature, err := s.signature(payload)
	if err != nil {
		return "", err
	}
	return payload + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (s *ActionSigner) Verify(token, purpose, subject string, now time.Time) (string, error) {
	if len(token) > 768 {
		return "", errors.New("invalid action token")
	}
	payload, signature, ok := strings.Cut(token, ".")
	if !ok {
		return "", errors.New("invalid action token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return "", err
	}
	expected, err := s.signature(payload)
	if err != nil {
		return "", err
	}
	if !hmac.Equal(sig, expected) {
		return "", errors.New("invalid action signature")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", err
	}
	parts := strings.Split(string(decoded), "\n")
	if len(parts) != 5 || parts[0] != "1" || parts[1] != purpose || parts[2] != subject || !IsUUIDv4(parts[4]) {
		return "", errors.New("action scope mismatch")
	}
	expires, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || now.Unix() >= expires {
		return "", errors.New("action expired")
	}
	return parts[4], nil
}
