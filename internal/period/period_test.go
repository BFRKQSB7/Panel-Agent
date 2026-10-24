package period

import (
	"testing"
	"time"
)

const testUser = "11111111-1111-4111-8111-111111111111"
const testNode = "22222222-2222-4222-8222-222222222222"
const testMachine = "33333333-3333-4333-8333-333333333333"

func instant(s string) time.Time {
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return v
}
func policy(t *testing.T, scope Scope, day int) Policy {
	t.Helper()
	p, err := New(scope, 1, instant("2024-01-01T00:00:00Z"), DefaultMonthly(day))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMonthlyThreeDimensionsShortMonthAndEdges(t *testing.T) {
	ps := []Policy{policy(t, Scope{Kind: "user", UserID: testUser}, 1), policy(t, Scope{Kind: "user_node", UserID: testUser, NodeID: testNode}, 15), policy(t, Scope{Kind: "machine", MachineID: testMachine}, 31)}
	ss, err := Split(ps, instant("2024-02-01T00:00:00Z"), instant("2024-03-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2024-02-15T00:00:00Z", "2024-02-29T00:00:00Z", "2024-03-01T00:00:00Z", "2024-03-02T00:00:00Z"}
	if len(ss) != len(want) {
		t.Fatal(ss)
	}
	for i, s := range ss {
		if !s.End.Equal(instant(want[i])) {
			t.Fatal(s)
		}
		seen := map[string]bool{}
		for _, p := range s.Periods {
			if seen[p.ID] {
				t.Fatal("shared dimensional ID")
			}
			seen[p.ID] = true
		}
	}
	for _, edge := range []string{"2024-02-29T00:00:00Z", "2025-02-28T00:00:00Z"} {
		at := instant(edge)
		before, _ := At(ps[2], at.Add(-time.Nanosecond))
		equal, _ := At(ps[2], at)
		after, _ := At(ps[2], at.Add(time.Nanosecond))
		if !before.End.Equal(at) || !equal.Start.Equal(at) || before.ID == equal.ID || equal.ID != after.ID {
			t.Fatal("left-closed/right-open broken")
		}
	}
}

func TestDSTExplicitGapFoldAndPinnedData(t *testing.T) {
	cases := []struct {
		zone      string
		y         int
		m         time.Month
		d, h, min int
		want      string
	}{
		{"America/New_York", 2024, 3, 10, 2, 30, "2024-03-10T07:00:00Z"},
		{"America/New_York", 2024, 11, 3, 1, 30, "2024-11-03T05:30:00Z"},
		{"Australia/Lord_Howe", 2024, 10, 6, 2, 15, "2024-10-05T15:30:00Z"},
		{"Pacific/Apia", 2011, 12, 30, 12, 0, "2011-12-30T10:00:00Z"},
	}
	t.Setenv("ZONEINFO", t.TempDir())
	for _, c := range cases {
		got, err := ResolveCivil(c.zone, c.y, c.m, c.d, c.h, c.min, 0)
		if err != nil || !got.Equal(instant(c.want)) {
			t.Fatalf("%s: %s %v", c.zone, got, err)
		}
	}
	p := policy(t, Scope{Kind: "user", UserID: testUser}, 10)
	p.Monthly.Timezone = "America/New_York"
	p.Monthly.Hour = 2
	p.Monthly.Minute = 30
	before, _ := At(p, instant("2024-03-10T06:59:59Z"))
	after, _ := At(p, instant("2024-03-10T07:00:00Z"))
	if !before.End.Equal(after.Start) || before.ID == after.ID {
		t.Fatal("DST period mismatch")
	}
	p.Monthly.TZDataID = "unavailable"
	if p.Validate() == nil {
		t.Fatal("unknown pinned data accepted")
	}
}

func TestPolicyResetOfflineSwitchAndRollback(t *testing.T) {
	old := policy(t, Scope{Kind: "user", UserID: testUser}, 1)
	reset, err := New(old.Scope, old.Revision, instant("2024-02-10T12:00:00Z"), old.Monthly)
	if err != nil {
		t.Fatal(err)
	}
	if CheckPublication([]Policy{old}, reset) != nil {
		t.Fatal("same revision new ID rejected")
	}
	changed := old
	changed.Monthly.Day = 2
	if CheckPublication([]Policy{old}, changed) == nil {
		t.Fatal("immutable publication overwritten")
	}
	online, err := Split([]Policy{old, reset}, instant("2024-02-09T00:00:00Z"), instant("2024-02-11T00:00:00Z"))
	if err != nil || len(online) != 2 || !online[0].End.Equal(reset.EffectiveAt) {
		t.Fatal(online, err)
	}
	oldPeriod, _ := At(old, instant("2024-02-11T00:00:00Z"))
	fixed, _ := At(reset, instant("2024-02-11T00:00:00Z"))
	late, _ := At(reset, instant("2024-02-15T00:00:00Z"))
	if oldPeriod.ID == fixed.ID || fixed.ID != late.ID || !fixed.Start.Equal(reset.EffectiveAt) || !oldPeriod.End.Equal(instant("2024-03-01T00:00:00Z")) {
		t.Fatal("machine apply time altered global period or old bounds")
	}
	offline, err := Split([]Policy{old}, instant("2024-02-09T00:00:00Z"), instant("2024-02-15T00:00:00Z"))
	if err != nil || len(offline) != 1 || offline[0].Periods[0].ID != oldPeriod.ID {
		t.Fatal(offline, err)
	}
	water := instant("2024-03-01T00:00:00Z")
	p, at, rewound, err := Resolve(reset, instant("2024-02-28T23:00:00Z"), water)
	if err != nil || !rewound || !at.Equal(water) || !p.Start.Equal(water) {
		t.Fatal("closed period reopened", p, err)
	}
	reset.Monthly.Day = 20
	reset.ID = "44444444-4444-4444-8444-444444444444"
	reset.EffectiveAt = instant("2024-02-18T10:00:00Z")
	s, err := Split([]Policy{old, reset}, instant("2024-02-17T00:00:00Z"), instant("2024-02-21T00:00:00Z"))
	if err != nil || len(s) != 3 {
		t.Fatal("policy rule change", s, err)
	}
}
