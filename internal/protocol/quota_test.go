package protocol

import (
	"testing"

	"panel-agent.local/panel-agent/internal/security"
)

func TestQuotaRestrictionsNormalizeAndMerge(t *testing.T) {
	user, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	node, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	first := QuotaRestrictions{Items: []QuotaRestriction{{UserID: user, NodeID: node, Reasons: []string{RestrictionNodeQuotaExhausted, RestrictionUserQuotaExhausted}}}}
	second := QuotaRestrictions{MachineStop: true, Items: []QuotaRestriction{{UserID: user, NodeID: node, Reasons: []string{RestrictionNodeQuotaExhausted}}}}
	merged, err := MergeQuotaRestrictions(first, second)
	if err != nil || !merged.MachineStop || len(merged.Items) != 1 || len(merged.Items[0].Reasons) != 2 {
		t.Fatalf("merged restrictions = %+v, %v", merged, err)
	}
	if merged.Items[0].Reasons[0] != RestrictionNodeQuotaExhausted || merged.Items[0].Reasons[1] != RestrictionUserQuotaExhausted {
		t.Fatalf("restriction reasons were not canonicalized: %+v", merged.Items[0].Reasons)
	}
	one, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	other, err := (QuotaRestrictions{Items: []QuotaRestriction{{UserID: user, NodeID: node, Reasons: []string{RestrictionUserQuotaExhausted, RestrictionNodeQuotaExhausted}}}}).Digest()
	if err != nil || one != other {
		t.Fatalf("equivalent restriction order changed digest: %q/%q", one, other)
	}
}

func TestQuotaRestrictionsRejectDuplicateOrUnknownReasons(t *testing.T) {
	user, _ := security.NewUUID()
	if err := (QuotaRestrictions{Items: []QuotaRestriction{{UserID: user, Reasons: []string{RestrictionUserQuotaExhausted, RestrictionUserQuotaExhausted}}}}).Validate(); err == nil {
		t.Fatal("duplicate restriction reason was accepted")
	}
	if err := (QuotaRestrictions{Items: []QuotaRestriction{{UserID: user, Reasons: []string{"unknown"}}}}).Validate(); err == nil {
		t.Fatal("unknown restriction reason was accepted")
	}
}
