package control

import (
	"errors"
	"sync"
	"time"
)

const (
	maxLoginFailures = 5
	maxTrackedIPs    = 4096
	loginWindow      = 15 * time.Minute
	loginLockout     = 15 * time.Minute
)

var errSessionCapacity = errors.New("session capacity reached")

type loginAttempt struct {
	failures    int
	lastAttempt time.Time
	blockedTill time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string]loginAttempt)}
}

func (l *loginLimiter) Allowed(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt, ok := l.attempts[key]
	if !ok {
		if len(l.attempts) >= maxTrackedIPs {
			l.prune(now)
			if len(l.attempts) >= maxTrackedIPs {
				return false, loginLockout
			}
		}
		return true, 0
	}
	if now.Before(attempt.blockedTill) {
		return false, attempt.blockedTill.Sub(now)
	}
	if now.Sub(attempt.lastAttempt) >= loginWindow {
		delete(l.attempts, key)
	}
	return true, 0
}

func (l *loginLimiter) Failed(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt := l.attempts[key]
	if now.Sub(attempt.lastAttempt) >= loginWindow {
		attempt.failures = 0
		attempt.blockedTill = time.Time{}
	}
	attempt.failures++
	attempt.lastAttempt = now
	if attempt.failures >= maxLoginFailures {
		attempt.blockedTill = now.Add(loginLockout)
	}
	l.attempts[key] = attempt
	if now.Before(attempt.blockedTill) {
		return attempt.blockedTill.Sub(now)
	}
	return 0
}

func (l *loginLimiter) Succeeded(key string) {
	l.mu.Lock()
	delete(l.attempts, key)
	l.mu.Unlock()
}

func (l *loginLimiter) prune(now time.Time) {
	for key, attempt := range l.attempts {
		if !now.Before(attempt.blockedTill) && now.Sub(attempt.lastAttempt) >= loginWindow {
			delete(l.attempts, key)
		}
	}
}
