package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"panel-agent.local/panel-agent/internal/security"
)

const (
	RestrictionUserQuotaExhausted     = "user_quota_exhausted"
	RestrictionNodeQuotaExhausted     = "node_quota_exhausted"
	RestrictionMachineBudgetExhausted = "machine_budget_exhausted"
)

type QuotaRestriction struct {
	UserID  string   `json:"user_id"`
	NodeID  string   `json:"node_id,omitempty"`
	Reasons []string `json:"reasons"`
}

// QuotaRestrictions is the enforcement part of a desired-state snapshot. It
// deliberately contains no usage counters, so soft-warning updates do not
// restart the Core.
type QuotaRestrictions struct {
	MachineStop bool               `json:"machine_stop,omitempty"`
	Items       []QuotaRestriction `json:"items,omitempty"`
}

func (r QuotaRestriction) key() string { return r.UserID + "\x00" + r.NodeID }

func validRestrictionReason(reason string) bool {
	switch reason {
	case RestrictionUserQuotaExhausted, RestrictionNodeQuotaExhausted, RestrictionMachineBudgetExhausted:
		return true
	default:
		return false
	}
}

func (r QuotaRestrictions) normalized() (QuotaRestrictions, error) {
	copy := QuotaRestrictions{MachineStop: r.MachineStop, Items: append([]QuotaRestriction(nil), r.Items...)}
	seen := map[string]bool{}
	for i := range copy.Items {
		item := &copy.Items[i]
		if !security.IsUUIDv4(item.UserID) || item.NodeID != "" && !security.IsUUIDv4(item.NodeID) || len(item.Reasons) == 0 || seen[item.key()] {
			return QuotaRestrictions{}, errors.New("额度限制身份或重复项无效")
		}
		seen[item.key()] = true
		item.Reasons = append([]string(nil), item.Reasons...)
		seenReasons := map[string]bool{}
		for _, reason := range item.Reasons {
			if !validRestrictionReason(reason) || seenReasons[reason] {
				return QuotaRestrictions{}, errors.New("额度限制原因无效")
			}
			seenReasons[reason] = true
		}
		sort.Strings(item.Reasons)
	}
	sort.Slice(copy.Items, func(i, j int) bool { return copy.Items[i].key() < copy.Items[j].key() })
	if len(copy.Items) == 0 {
		copy.Items = nil
	}
	return copy, nil
}

func (r QuotaRestrictions) Validate() error {
	_, err := r.normalized()
	return err
}

func (r QuotaRestrictions) Digest() (string, error) {
	normalized, err := r.normalized()
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func MergeQuotaRestrictions(first, second QuotaRestrictions) (QuotaRestrictions, error) {
	merged := QuotaRestrictions{MachineStop: first.MachineStop || second.MachineStop}
	byKey := map[string]map[string]bool{}
	for _, source := range []QuotaRestrictions{first, second} {
		if err := source.Validate(); err != nil {
			return QuotaRestrictions{}, err
		}
		for _, item := range source.Items {
			if byKey[item.key()] == nil {
				byKey[item.key()] = map[string]bool{}
			}
			for _, reason := range item.Reasons {
				byKey[item.key()][reason] = true
			}
		}
	}
	for key, reasons := range byKey {
		separator := -1
		for i := range key {
			if key[i] == 0 {
				separator = i
				break
			}
		}
		if separator < 0 {
			return QuotaRestrictions{}, errors.New("额度限制键无效")
		}
		item := QuotaRestriction{UserID: key[:separator], NodeID: key[separator+1:]}
		for reason := range reasons {
			item.Reasons = append(item.Reasons, reason)
		}
		merged.Items = append(merged.Items, item)
	}
	return merged.normalized()
}
