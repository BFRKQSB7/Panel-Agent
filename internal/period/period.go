// Package period is the shared, offline-capable Panel/Agent billing calendar.
package period

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"panel-agent.local/panel-agent/internal/security"
)

var ErrPolicy = errors.New("无效或冲突的周期策略")

type Scope struct {
	Kind      string `json:"kind"`
	UserID    string `json:"user_id,omitempty"`
	NodeID    string `json:"node_id,omitempty"`
	MachineID string `json:"machine_id,omitempty"`
}

func (s Scope) Validate() error {
	switch s.Kind {
	case "user":
		if security.IsUUIDv4(s.UserID) && s.NodeID == "" && s.MachineID == "" {
			return nil
		}
	case "user_node":
		if security.IsUUIDv4(s.UserID) && security.IsUUIDv4(s.NodeID) && s.MachineID == "" {
			return nil
		}
	case "machine":
		if security.IsUUIDv4(s.MachineID) && s.UserID == "" && s.NodeID == "" {
			return nil
		}
	}
	return ErrPolicy
}
func (s Scope) Key() string { b, _ := json.Marshal(s); return string(b) }

type Monthly struct {
	Day      int    `json:"day"`
	Hour     int    `json:"hour"`
	Minute   int    `json:"minute"`
	Second   int    `json:"second"`
	Timezone string `json:"timezone"`
	TZDataID string `json:"tzdata_id"`
}

func DefaultMonthly(day int) Monthly { return Monthly{Day: day, Timezone: "UTC", TZDataID: TZDataID} }

type Policy struct {
	Scope       Scope     `json:"scope"`
	ID          string    `json:"policy_id"`
	Revision    uint64    `json:"policy_revision"`
	EffectiveAt time.Time `json:"effective_at"`
	Monthly     Monthly   `json:"monthly"`
}

// New also serves manual reset: always allocate a fresh, non-reused identity.
func New(scope Scope, revision uint64, effective time.Time, rule Monthly) (Policy, error) {
	id, err := security.NewUUID()
	if err != nil {
		return Policy{}, err
	}
	p := Policy{scope, id, revision, effective.UTC(), rule}
	return p, p.Validate()
}

func ValidUTC(t time.Time) bool {
	_, offset := t.Zone()
	return !t.IsZero() && offset == 0 && t.Year() >= 1970 && t.Year() <= 9998
}

func (p Policy) Validate() error {
	r := p.Monthly
	if p.Scope.Validate() != nil || !security.IsUUIDv4(p.ID) || p.Revision == 0 || p.Revision > 1<<63-1 || !ValidUTC(p.EffectiveAt) || r.Day < 1 || r.Day > 31 || r.Hour < 0 || r.Hour > 23 || r.Minute < 0 || r.Minute > 59 || r.Second < 0 || r.Second > 59 || r.TZDataID != TZDataID {
		return ErrPolicy
	}
	_, err := location(r.Timezone)
	return err
}

// CheckPublication prevents reusing an ID with changed content. Revisions may
// repeat after restore; they never replace policy identity or effective_at.
func CheckPublication(existing []Policy, candidate Policy) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	for _, p := range existing {
		if p.ID == candidate.ID && !samePolicy(p, candidate) {
			return ErrPolicy
		}
		if p.Scope == candidate.Scope && p.EffectiveAt.Equal(candidate.EffectiveAt) && p.ID != candidate.ID {
			return ErrPolicy
		}
	}
	return nil
}
func samePolicy(a, b Policy) bool {
	return a.Scope == b.Scope && a.ID == b.ID && a.Revision == b.Revision && a.EffectiveAt.Equal(b.EffectiveAt) && a.Monthly == b.Monthly
}

type Period struct {
	ID             string    `json:"period_id"`
	Scope          Scope     `json:"scope"`
	PolicyID       string    `json:"policy_id"`
	PolicyRevision uint64    `json:"policy_revision"`
	Start          time.Time `json:"start_utc"`
	End            time.Time `json:"end_utc"`
}

func (p Period) identity() string {
	b, _ := json.Marshal([]string{"period-v1", p.Scope.Key(), p.PolicyID, formatRevision(p.PolicyRevision), p.Start.UTC().Format(time.RFC3339Nano), p.End.UTC().Format(time.RFC3339Nano)})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (p Period) Validate() error {
	if p.Scope.Validate() != nil || !security.IsUUIDv4(p.PolicyID) || p.PolicyRevision == 0 || p.PolicyRevision > 1<<63-1 || !ValidUTC(p.Start) || !ValidUTC(p.End) || !p.Start.Before(p.End) || p.ID != p.identity() {
		return ErrPolicy
	}
	return nil
}
func formatRevision(n uint64) string { b, _ := json.Marshal(n); return string(b) }

// At returns immutable scheduled bounds. A later policy does not rewrite an
// old period's end: offline segments may still legitimately belong to it.
func At(policy Policy, instant time.Time) (Period, error) {
	if err := policy.Validate(); err != nil {
		return Period{}, err
	}
	if !ValidUTC(instant.UTC()) || instant.Before(policy.EffectiveAt) {
		return Period{}, ErrPolicy
	}
	loc, _ := location(policy.Monthly.Timezone)
	local := instant.In(loc)
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, time.UTC)
	start, err := boundary(policy.Monthly, month)
	if err != nil {
		return Period{}, err
	}
	if instant.Before(start) {
		month = month.AddDate(0, -1, 0)
		start, err = boundary(policy.Monthly, month)
		if err != nil {
			return Period{}, err
		}
	}
	end, err := boundary(policy.Monthly, month.AddDate(0, 1, 0))
	if err != nil {
		return Period{}, err
	}
	if policy.EffectiveAt.After(start) {
		start = policy.EffectiveAt
	}
	p := Period{Scope: policy.Scope, PolicyID: policy.ID, PolicyRevision: policy.Revision, Start: start.UTC(), End: end.UTC()}
	p.ID = p.identity()
	return p, p.Validate()
}

// Resolve clamps accounting time to a persisted high-water mark. D08 must save
// the returned mark atomically with segment closure; wall-clock rollback cannot
// reopen closed periods. This is not a correction of the physical clock.
func Resolve(policy Policy, wall, highWater time.Time) (Period, time.Time, bool, error) {
	at := wall.UTC()
	rewound := !highWater.IsZero() && at.Before(highWater)
	if rewound {
		at = highWater.UTC()
	}
	p, err := At(policy, at)
	return p, at, rewound, err
}

type Slice struct {
	Start   time.Time `json:"start_utc"`
	End     time.Time `json:"end_utc"`
	Periods []Period  `json:"periods"`
}

// Split intersects known schedules. It never interpolates unobserved bytes.
// Offline callers provide only policies they actually received. On sync, the
// final old-policy sample precedes applying the new policy at the machine.
func Split(policies []Policy, start, end time.Time) ([]Slice, error) {
	if len(policies) == 0 || len(policies) > 4096 || !ValidUTC(start) || !ValidUTC(end) || !start.Before(end) {
		return nil, ErrPolicy
	}
	groups := map[string][]Policy{}
	for i, p := range policies {
		if err := CheckPublication(policies[:i], p); err != nil {
			return nil, err
		}
		groups[p.Scope.Key()] = append(groups[p.Scope.Key()], p)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sort.Slice(groups[k], func(i, j int) bool { return groups[k][i].EffectiveAt.Before(groups[k][j].EffectiveAt) })
	}
	result := []Slice{}
	for cursor := start.UTC(); cursor.Before(end); {
		if len(result) >= 4096 {
			return nil, ErrPolicy
		}
		s := Slice{Start: cursor, End: end.UTC()}
		for _, k := range keys {
			ps := groups[k]
			idx := -1
			for i, p := range ps {
				if !p.EffectiveAt.After(cursor) {
					idx = i
				} else {
					break
				}
			}
			if idx < 0 {
				return nil, ErrPolicy
			}
			p, err := At(ps[idx], cursor)
			if err != nil {
				return nil, err
			}
			s.Periods = append(s.Periods, p)
			if p.End.Before(s.End) {
				s.End = p.End
			}
			if idx+1 < len(ps) && ps[idx+1].EffectiveAt.Before(s.End) {
				s.End = ps[idx+1].EffectiveAt
			}
		}
		if !cursor.Before(s.End) {
			return nil, ErrPolicy
		}
		result = append(result, s)
		cursor = s.End
	}
	return result, nil
}
