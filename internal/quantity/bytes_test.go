package quantity

import (
	"encoding/json"
	"math"
	"testing"
)

func TestExactWireAmountsAndCheckedAddition(t *testing.T) {
	for _, s := range []string{"0", "9007199254740993", "9223372036854775807"} {
		var b Bytes
		if err := json.Unmarshal([]byte(`"`+s+`"`), &b); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(b)
		if err != nil || string(encoded) != `"`+s+`"` {
			t.Fatal(s, string(encoded), err)
		}
	}
	for _, s := range []string{`0`, `null`, `"-1"`, `"01"`, `"+1"`, `"1.0"`, `"1e3"`, `"9223372036854775808"`, `"\u0031"`, `" 1"`, `""`} {
		var b Bytes
		if json.Unmarshal([]byte(s), &b) == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	var q struct {
		Unlimited *Bytes `json:"unlimited"`
		Zero      *Bytes `json:"zero"`
	}
	if err := json.Unmarshal([]byte(`{"unlimited":null,"zero":"0"}`), &q); err != nil || q.Unlimited != nil || q.Zero == nil || *q.Zero != 0 {
		t.Fatal("null/zero conflation", err)
	}
	if n, err := Add(math.MaxInt64-1, 1); err != nil || n != math.MaxInt64 {
		t.Fatal(n, err)
	}
	if _, err := Add(math.MaxInt64, 1); err == nil {
		t.Fatal("overflow accepted")
	}
	if _, err := Add(-1, 1); err == nil {
		t.Fatal("negative accepted")
	}
	if _, err := json.Marshal(Bytes(-1)); err == nil {
		t.Fatal("negative serialized")
	}
}
