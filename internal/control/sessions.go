package control

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"
)

const sessionLifetime = 12 * time.Hour

type sessionRecord struct {
	csrf      string
	expiresAt time.Time
}

type sessionManager struct {
	mu       sync.Mutex
	sessions map[[32]byte]sessionRecord
}

func newSessionManager() *sessionManager {
	return &sessionManager{sessions: make(map[[32]byte]sessionRecord)}
}

func newRandomToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

func (m *sessionManager) Create(now time.Time) (string, sessionRecord, error) {
	var tokenBytes [32]byte
	var csrfBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return "", sessionRecord{}, err
	}
	if _, err := rand.Read(csrfBytes[:]); err != nil {
		return "", sessionRecord{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes[:])
	record := sessionRecord{
		csrf:      base64.RawURLEncoding.EncodeToString(csrfBytes[:]),
		expiresAt: now.Add(sessionLifetime),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for digest, existing := range m.sessions {
		if !now.Before(existing.expiresAt) {
			delete(m.sessions, digest)
		}
	}
	if len(m.sessions) >= 4096 {
		return "", sessionRecord{}, errSessionCapacity
	}
	m.sessions[sha256.Sum256(tokenBytes[:])] = record
	return token, record, nil
}

func (m *sessionManager) Get(token string, now time.Time) (sessionRecord, bool) {
	tokenBytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(tokenBytes) != 32 {
		return sessionRecord{}, false
	}
	digest := sha256.Sum256(tokenBytes)
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.sessions[digest]
	if !ok {
		return sessionRecord{}, false
	}
	if !now.Before(record.expiresAt) {
		delete(m.sessions, digest)
		return sessionRecord{}, false
	}
	return record, true
}

func (m *sessionManager) Delete(token string) {
	tokenBytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(tokenBytes) != 32 {
		return
	}
	digest := sha256.Sum256(tokenBytes)
	m.mu.Lock()
	delete(m.sessions, digest)
	m.mu.Unlock()
}
