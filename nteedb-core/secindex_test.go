package nteedb

import "testing"

func TestSecIndexExactMultiValue(t *testing.T) {
	si := newSecIndex(IndexDef{Name: "traceId", Kind: KindString})
	for _, pk := range []string{"r3", "r1", "r2"} {
		e, _ := si.makeEntry("abc", pk)
		si.insert(e)
	}
	e, _ := si.makeEntry("xyz", "r4")
	si.insert(e)

	got, err := si.exact("abc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrs(got, []string{"r1", "r2", "r3"}) {
		t.Errorf("exact(abc) = %v, want [r1 r2 r3] sorted by pk", got)
	}
	if got, _ := si.exact("xyz", 0); !eqStrs(got, []string{"r4"}) {
		t.Errorf("exact(xyz) = %v", got)
	}
	if got, _ := si.exact("none", 0); len(got) != 0 {
		t.Errorf("exact(none) = %v, want empty", got)
	}
}

func TestSecIndexExactLimitAndDirection(t *testing.T) {
	si := newSecIndex(IndexDef{Name: "traceId", Kind: KindString})
	// 6 entries under "T" with ordered pks, plus one other value.
	for _, pk := range []string{"k1", "k2", "k3", "k4", "k5", "k6"} {
		e, _ := si.makeEntry("T", pk)
		si.insert(e)
	}
	e, _ := si.makeEntry("other", "z")
	si.insert(e)

	// limit 0 → all ascending
	if got, _ := si.exact("T", 0); !eqStrs(got, []string{"k1", "k2", "k3", "k4", "k5", "k6"}) {
		t.Errorf("limit 0 = %v", got)
	}
	// limit 3 → first 3 ascending
	if got, _ := si.exact("T", 3); !eqStrs(got, []string{"k1", "k2", "k3"}) {
		t.Errorf("limit 3 = %v, want [k1 k2 k3]", got)
	}
	// limit -2 → last 2 descending
	if got, _ := si.exact("T", -2); !eqStrs(got, []string{"k6", "k5"}) {
		t.Errorf("limit -2 = %v, want [k6 k5]", got)
	}
	// limit beyond count clamps
	if got, _ := si.exact("T", 100); len(got) != 6 {
		t.Errorf("limit 100 = %v, want all 6", got)
	}
	if got, _ := si.exact("T", -100); !eqStrs(got, []string{"k6", "k5", "k4", "k3", "k2", "k1"}) {
		t.Errorf("limit -100 = %v, want all 6 desc", got)
	}
	// absent value → empty regardless of limit
	if got, _ := si.exact("nope", -5); len(got) != 0 {
		t.Errorf("absent value = %v, want empty", got)
	}
}

func TestSecIndexRemove(t *testing.T) {
	si := newSecIndex(IndexDef{Name: "t", Kind: KindString})
	for _, pk := range []string{"a", "b", "c"} {
		e, _ := si.makeEntry("v", pk)
		si.insert(e)
	}
	rm, _ := si.makeEntry("v", "b")
	si.remove(rm)
	if got, _ := si.exact("v", 0); !eqStrs(got, []string{"a", "c"}) {
		t.Errorf("after remove = %v, want [a c]", got)
	}
}

func TestSecIndexNumberRange(t *testing.T) {
	si := newSecIndex(IndexDef{Name: "status", Kind: KindNumber})
	data := map[string]float64{"ok1": 200, "ok2": 204, "redir": 301, "err": 500}
	for pk, v := range data {
		e, _ := si.makeEntry(v, pk)
		si.insert(e)
	}
	got, err := si.rangeQuery(200, 299)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrs(got, []string{"ok1", "ok2"}) {
		t.Errorf("range 200-299 = %v, want [ok1 ok2]", got)
	}
	if got, _ := si.rangeQuery(300, 599); !eqStrs(got, []string{"redir", "err"}) {
		t.Errorf("range 300-599 = %v, want [redir err]", got)
	}
}

func TestSecIndexStringPrefix(t *testing.T) {
	si := newSecIndex(IndexDef{Name: "name", Kind: KindString})
	for _, v := range []struct{ val, pk string }{
		{"GetOrders", "k1"}, {"GetProperty", "k2"}, {"SetX", "k3"},
	} {
		e, _ := si.makeEntry(v.val, v.pk)
		si.insert(e)
	}
	got, err := si.prefix("Get", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrs(got, []string{"k1", "k2"}) {
		t.Errorf("prefix(Get) = %v, want [k1 k2]", got)
	}
}

// TestSecIndexPrefixWindow exercises the binary-search match window edges: an
// empty prefix (no upper bound — matches everything), and a prefix whose lower
// bound lands on a larger, non-matching value (must return empty, not that row).
func TestSecIndexPrefixWindow(t *testing.T) {
	si := newSecIndex(IndexDef{Name: "name", Kind: KindString})
	for _, v := range []struct{ val, pk string }{
		{"Get", "k1"}, {"GetX", "k2"}, {"Gf", "k3"},
	} {
		e, _ := si.makeEntry(v.val, v.pk)
		si.insert(e)
	}

	// Empty prefix matches every entry (prefixUpperBound reports no bound).
	if got, _ := si.prefix("", 0); !eqStrs(got, []string{"k1", "k2", "k3"}) {
		t.Errorf("prefix(\"\") = %v, want all", got)
	}
	// "Get" excludes "Gf": lo lands on "Get", hi is the first value >= "Geu",
	// which is "Gf", so the window is exactly the two "Get*" rows.
	if got, _ := si.prefix("Get", 0); !eqStrs(got, []string{"k1", "k2"}) {
		t.Errorf("prefix(Get) = %v, want [k1 k2]", got)
	}
	// "Gg" sorts after every entry, so lo == hi == len and the window is empty.
	if got, _ := si.prefix("Gg", 0); len(got) != 0 {
		t.Errorf("prefix(Gg) = %v, want empty", got)
	}
	// "Gem" matches nothing, yet its lower bound lands on the non-matching row
	// "Get" (since "Gem" < "Get"). The successor bound "Gen" makes hi land on
	// "Get" too, so the window is correctly empty — this guards against lo
	// leaking a larger, non-prefixed row when there is no exact prefix match.
	if got, _ := si.prefix("Gem", 0); len(got) != 0 {
		t.Errorf("prefix(Gem) = %v, want empty", got)
	}
	// A prefix longer than any value and below all of them → empty.
	if got, _ := si.prefix("A", -1); len(got) != 0 {
		t.Errorf("prefix(A) = %v, want empty", got)
	}
}

// TestSecIndexPrefixGroupedLimit checks that a prefix spanning multiple distinct
// values applies the limit per value (grouped), not to the flattened list.
func TestSecIndexPrefixGroupedLimit(t *testing.T) {
	si := newSecIndex(IndexDef{Name: "endpoint", Kind: KindString})
	// Two records under GetXXXMutation, one under GetXXXMumu. Sorted by
	// (value, pk): GetXXXMumu < GetXXXMutation ('m' < 't').
	for _, v := range []struct{ val, pk string }{
		{"GetXXXMutation", "1"}, {"GetXXXMutation", "2"}, {"GetXXXMumu", "3"}, {"SetX", "4"},
	} {
		e, _ := si.makeEntry(v.val, v.pk)
		si.insert(e)
	}

	// limit 0: all matches, flat, in (value, pk) order.
	if got, _ := si.prefix("GetXXXM", 0); !eqStrs(got, []string{"3", "1", "2"}) {
		t.Errorf("prefix(GetXXXM, 0) = %v, want [3 1 2]", got)
	}
	// limit -1: last of each value, groups ascending by value.
	if got, _ := si.prefix("GetXXXM", -1); !eqStrs(got, []string{"3", "2"}) {
		t.Errorf("prefix(GetXXXM, -1) = %v, want [3 2]", got)
	}
	// limit +1: first of each value.
	if got, _ := si.prefix("GetXXXM", 1); !eqStrs(got, []string{"3", "1"}) {
		t.Errorf("prefix(GetXXXM, 1) = %v, want [3 1]", got)
	}
	// limit -2: last 2 of each value descending; GetXXXMumu has only 1.
	if got, _ := si.prefix("GetXXXM", -2); !eqStrs(got, []string{"3", "2", "1"}) {
		t.Errorf("prefix(GetXXXM, -2) = %v, want [3 2 1]", got)
	}
}

func TestSecIndexTypeMismatch(t *testing.T) {
	si := newSecIndex(IndexDef{Name: "status", Kind: KindNumber})
	if _, err := si.makeEntry("not-a-number", "k"); err == nil {
		t.Error("expected error for string into number index")
	}
	str := newSecIndex(IndexDef{Name: "name", Kind: KindString})
	if _, err := str.makeEntry(42, "k"); err == nil {
		t.Error("expected error for number into string index")
	}
	if _, err := str.prefix("x", 0); err != nil {
		t.Errorf("prefix on string index should be fine: %v", err)
	}
	if _, err := si.prefix("x", 0); err == nil {
		t.Error("prefix on number index should error")
	}
}

func TestToFloatAcceptsNumericTypes(t *testing.T) {
	for _, v := range []any{200, int64(200), int32(200), float32(200), float64(200)} {
		if f, ok := toFloat(v); !ok || f != 200 {
			t.Errorf("toFloat(%T) = %v %v", v, f, ok)
		}
	}
	if _, ok := toFloat("200"); ok {
		t.Error("toFloat should reject strings")
	}
}
