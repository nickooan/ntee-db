package nteedb

import (
	"encoding/json"
	"fmt"
	"testing"
)

func openIndexed(t *testing.T, dir string) *DB {
	t.Helper()
	db, err := Open(Options{
		Dir: dir,
		Indexes: []IndexDef{
			{Name: "traceId", Kind: KindString},
			{Name: "status", Kind: KindNumber},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

func mustBy(t *testing.T, db *DB, name string, val any) []string {
	t.Helper()
	got, err := db.ByIndex(name, val)
	if err != nil {
		t.Fatalf("ByIndex(%s,%v): %v", name, val, err)
	}
	return got
}

func TestByIndexHas(t *testing.T) {
	db := openIndexed(t, t.TempDir())
	defer db.Close()

	db.PutIndexed("call:1", []byte(`{"v":"a"}`), IndexValues{"traceId": "T1", "status": 200})
	db.PutIndexed("call:2", []byte(`{"v":"b"}`), IndexValues{"traceId": "T1", "status": 404})

	check := func(name string, val any, want bool) {
		t.Helper()
		got, err := db.ByIndexHas(name, val)
		if err != nil {
			t.Fatalf("ByIndexHas(%s,%v): %v", name, val, err)
		}
		if got != want {
			t.Errorf("ByIndexHas(%s,%v) = %v, want %v", name, val, got, want)
		}
	}
	check("traceId", "T1", true)
	check("traceId", "T9", false)
	check("status", 404, true)
	check("status", 500, false)

	// Unknown index is an error.
	if _, err := db.ByIndexHas("nope", "x"); err == nil {
		t.Error("ByIndexHas(unknown) should error")
	}

	// Presence tracks deletes: gone once every record for the value is removed.
	db.Delete("call:1")
	check("traceId", "T1", true) // call:2 still has T1
	db.Delete("call:2")
	check("traceId", "T1", false)
}

func TestSecondaryMultiValueTraceId(t *testing.T) {
	db := openIndexed(t, t.TempDir())
	defer db.Close()

	db.PutIndexed("call:1", []byte(`{"v":"a"}`), IndexValues{"traceId": "T1", "status": 200})
	db.PutIndexed("call:2", []byte(`{"v":"b"}`), IndexValues{"traceId": "T1", "status": 404})
	db.PutIndexed("call:3", []byte(`{"v":"c"}`), IndexValues{"traceId": "T2", "status": 200})

	if got := mustBy(t, db, "traceId", "T1"); !eqStrs(got, []string{"call:1", "call:2"}) {
		t.Errorf("traceId T1 = %v, want [call:1 call:2]", got)
	}
	if got := mustBy(t, db, "traceId", "T2"); !eqStrs(got, []string{"call:3"}) {
		t.Errorf("traceId T2 = %v", got)
	}

	// Deleting one record retracts it from the index.
	db.Delete("call:1")
	if got := mustBy(t, db, "traceId", "T1"); !eqStrs(got, []string{"call:2"}) {
		t.Errorf("after delete traceId T1 = %v, want [call:2]", got)
	}
}

func TestSecondaryOverwriteRetraction(t *testing.T) {
	db := openIndexed(t, t.TempDir())
	defer db.Close()

	db.PutIndexed("k", []byte(`{"v":"v1"}`), IndexValues{"traceId": "OLD"})
	if got := mustBy(t, db, "traceId", "OLD"); !eqStrs(got, []string{"k"}) {
		t.Fatalf("OLD = %v", got)
	}

	// Overwrite with a new index value: the old mapping must disappear.
	db.PutIndexed("k", []byte(`{"v":"v2"}`), IndexValues{"traceId": "NEW"})
	if got := mustBy(t, db, "traceId", "OLD"); len(got) != 0 {
		t.Errorf("OLD should be empty after overwrite, got %v", got)
	}
	if got := mustBy(t, db, "traceId", "NEW"); !eqStrs(got, []string{"k"}) {
		t.Errorf("NEW = %v, want [k]", got)
	}
}

func TestSecondaryNumberRangeAndPrefix(t *testing.T) {
	db := openIndexed(t, t.TempDir())
	defer db.Close()

	db.PutIndexed("a", []byte(`{"v":"x"}`), IndexValues{"status": 200, "traceId": "GetOrders"})
	db.PutIndexed("b", []byte(`{"v":"x"}`), IndexValues{"status": 204, "traceId": "GetProperty"})
	db.PutIndexed("c", []byte(`{"v":"x"}`), IndexValues{"status": 500, "traceId": "SetX"})

	got, err := db.ByIndexRange("status", 200, 299)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrs(got, []string{"a", "b"}) {
		t.Errorf("status range 200-299 = %v, want [a b]", got)
	}

	pre, err := db.ByIndexPrefix("traceId", "Get")
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrs(pre, []string{"a", "b"}) {
		t.Errorf("traceId prefix Get = %v, want [a b]", pre)
	}
}

// TestSecondaryPrefixGroupedLimit exercises the grouped +/-N limit end-to-end:
// a prefix spanning two endpoints returns the latest (or first) record of each.
func TestSecondaryPrefixGroupedLimit(t *testing.T) {
	db := openIndexed(t, t.TempDir())
	defer db.Close()

	// GetXXXMutation has two records; GetXXXMumu one. Primary keys encode order.
	db.PutIndexed("call:1", []byte(`{"v":"x"}`), IndexValues{"traceId": "GetXXXMutation"})
	db.PutIndexed("call:2", []byte(`{"v":"x"}`), IndexValues{"traceId": "GetXXXMutation"})
	db.PutIndexed("call:3", []byte(`{"v":"x"}`), IndexValues{"traceId": "GetXXXMumu"})

	// -1: last record of each endpoint (groups ascending by value).
	if got, _ := db.ByIndexPrefix("traceId", "GetXXXM", -1); !eqStrs(got, []string{"call:3", "call:2"}) {
		t.Errorf("prefix GetXXXM limit -1 = %v, want [call:3 call:2]", got)
	}
	// 0: full flat collection in (value, pk) order.
	if got, _ := db.ByIndexPrefix("traceId", "GetXXXM", 0); !eqStrs(got, []string{"call:3", "call:1", "call:2"}) {
		t.Errorf("prefix GetXXXM limit 0 = %v, want [call:3 call:1 call:2]", got)
	}
	// Omitted limit behaves like 0 (all).
	if got, _ := db.ByIndexPrefix("traceId", "GetXXXM"); !eqStrs(got, []string{"call:3", "call:1", "call:2"}) {
		t.Errorf("prefix GetXXXM no limit = %v, want [call:3 call:1 call:2]", got)
	}
}

func TestRemoveByPkRangeDelete(t *testing.T) {
	db := openIndexed(t, t.TempDir())
	defer db.Close()

	// Five time-ordered keys, each with a distinct traceId secondary value.
	for _, c := range []struct{ key, trace string }{
		{"call:1", "T1"}, {"call:2", "T2"}, {"call:3", "T3"},
		{"call:4", "T4"}, {"call:5", "T5"},
	} {
		db.PutIndexed(c.key, []byte(`{"v":"v"}`), IndexValues{"traceId": c.trace})
	}

	// RemoveByPkLess is strict: "call:3" itself survives; call:1/2 go.
	n, err := db.RemoveByPkLess("call:3")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("RemoveByPkLess count = %d, want 2", n)
	}
	if db.Has("call:1") || db.Has("call:2") {
		t.Error("call:1/call:2 should be gone from the primary index")
	}
	if !db.Has("call:3") {
		t.Error("call:3 (the cutoff) must survive a strict less-than delete")
	}
	// Secondary entries for the deleted keys must be swept (no ghosts).
	if got := mustBy(t, db, "traceId", "T1"); len(got) != 0 {
		t.Errorf("traceId T1 after delete = %v, want empty", got)
	}
	if got := mustBy(t, db, "traceId", "T3"); !eqStrs(got, []string{"call:3"}) {
		t.Errorf("traceId T3 = %v, want [call:3]", got)
	}

	// RemoveByPkGreater is strict too: "call:3" survives; call:4/5 go.
	n, err = db.RemoveByPkGreater("call:3")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("RemoveByPkGreater count = %d, want 2", n)
	}
	if db.Has("call:4") || db.Has("call:5") {
		t.Error("call:4/call:5 should be gone")
	}
	if !db.Has("call:3") {
		t.Error("call:3 must survive a strict greater-than delete")
	}
	if got := mustBy(t, db, "traceId", "T5"); len(got) != 0 {
		t.Errorf("traceId T5 after delete = %v, want empty", got)
	}

	// A no-op range (nothing strictly less than the smallest key) removes nothing.
	if n, _ := db.RemoveByPkLess("call:3"); n != 0 {
		t.Errorf("no-op RemoveByPkLess count = %d, want 0", n)
	}
}

func TestRemoveByPkRangeDurableAfterReopen(t *testing.T) {
	dir := t.TempDir()
	db := openIndexed(t, dir)
	for _, c := range []struct{ key, trace string }{
		{"call:1", "T1"}, {"call:2", "T2"}, {"call:3", "T3"},
	} {
		db.PutIndexed(c.key, []byte(`{"v":"v"}`), IndexValues{"traceId": c.trace})
	}
	if _, err := db.RemoveByPkLess("call:3"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopen: the deletions (tombstones + hint) must survive, primary and
	// secondary alike.
	db2 := openIndexed(t, dir)
	defer db2.Close()
	if db2.Has("call:1") || db2.Has("call:2") {
		t.Error("deleted keys resurfaced after reopen")
	}
	if !db2.Has("call:3") {
		t.Error("call:3 should still exist after reopen")
	}
	if got := mustBy(t, db2, "traceId", "T1"); len(got) != 0 {
		t.Errorf("after reopen traceId T1 = %v, want empty", got)
	}
	if got := mustBy(t, db2, "traceId", "T3"); !eqStrs(got, []string{"call:3"}) {
		t.Errorf("after reopen traceId T3 = %v, want [call:3]", got)
	}
}

// openCapped opens a store whose traceId index caps records per value.
func openCapped(t *testing.T, dir string, cap int) *DB {
	t.Helper()
	db, err := Open(Options{
		Dir: dir,
		Indexes: []IndexDef{
			{Name: "traceId", Kind: KindString, MaxPerValue: cap},
			{Name: "status", Kind: KindNumber},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

func TestMaxPerValueEvictsOldest(t *testing.T) {
	db := openCapped(t, t.TempDir(), 2)
	defer db.Close()

	db.PutIndexed("call:1", []byte(`{"v":"a"}`), IndexValues{"traceId": "T", "status": 200})
	db.PutIndexed("call:2", []byte(`{"v":"b"}`), IndexValues{"traceId": "T", "status": 201})
	// Third record for the same value: the lowest pk (call:1) is evicted.
	db.PutIndexed("call:3", []byte(`{"v":"c"}`), IndexValues{"traceId": "T", "status": 202})

	if db.Has("call:1") {
		t.Error("call:1 should be fully deleted after exceeding the cap")
	}
	if got := mustBy(t, db, "traceId", "T"); !eqStrs(got, []string{"call:2", "call:3"}) {
		t.Errorf("traceId T = %v, want [call:2 call:3]", got)
	}
	// Cross-index cascade: the evicted record's status entry is gone too.
	if got := mustBy(t, db, "status", 200); len(got) != 0 {
		t.Errorf("status 200 = %v, want empty (evicted record cascades)", got)
	}
	if got := mustBy(t, db, "status", 201); !eqStrs(got, []string{"call:2"}) {
		t.Errorf("status 201 = %v, want [call:2]", got)
	}

	// A fourth record rolls the window again.
	db.PutIndexed("call:4", []byte(`{"v":"d"}`), IndexValues{"traceId": "T", "status": 203})
	if got := mustBy(t, db, "traceId", "T"); !eqStrs(got, []string{"call:3", "call:4"}) {
		t.Errorf("traceId T = %v, want [call:3 call:4]", got)
	}
}

func TestMaxPerValueScopedPerValueAndOverwrite(t *testing.T) {
	db := openCapped(t, t.TempDir(), 2)
	defer db.Close()

	// Distinct values each get their own budget — no cross-value eviction.
	db.PutIndexed("a:1", []byte(`{"v":"x"}`), IndexValues{"traceId": "A"})
	db.PutIndexed("a:2", []byte(`{"v":"x"}`), IndexValues{"traceId": "A"})
	db.PutIndexed("b:1", []byte(`{"v":"x"}`), IndexValues{"traceId": "B"})
	if got := mustBy(t, db, "traceId", "A"); !eqStrs(got, []string{"a:1", "a:2"}) {
		t.Errorf("traceId A = %v", got)
	}

	// Overwriting an existing pk keeps the group at size 2 — nothing evicted.
	db.PutIndexed("a:2", []byte(`{"v":"y"}`), IndexValues{"traceId": "A"})
	if got := mustBy(t, db, "traceId", "A"); !eqStrs(got, []string{"a:1", "a:2"}) {
		t.Errorf("after overwrite traceId A = %v, want [a:1 a:2]", got)
	}
	if !db.Has("a:1") {
		t.Error("overwrite must not evict a:1")
	}
}

func TestMaxPerValueUnlimitedByDefault(t *testing.T) {
	db := openIndexed(t, t.TempDir()) // no MaxPerValue set anywhere
	defer db.Close()
	for i := 1; i <= 6; i++ {
		db.PutIndexed(fmt.Sprintf("call:%d", i), []byte(`{"v":"x"}`), IndexValues{"traceId": "T"})
	}
	if got := mustBy(t, db, "traceId", "T"); len(got) != 6 {
		t.Errorf("unlimited index kept %d records, want 6", len(got))
	}
}

func TestMaxPerValueDurableAndSelfHealing(t *testing.T) {
	dir := t.TempDir()
	db := openCapped(t, dir, 3)
	db.PutIndexed("call:1", []byte(`{"v":"x"}`), IndexValues{"traceId": "T"})
	db.PutIndexed("call:2", []byte(`{"v":"x"}`), IndexValues{"traceId": "T"})
	db.PutIndexed("call:3", []byte(`{"v":"x"}`), IndexValues{"traceId": "T"})
	db.PutIndexed("call:4", []byte(`{"v":"x"}`), IndexValues{"traceId": "T"}) // evicts call:1
	db.Close()

	// Reopen with the SAME cap: the eviction survived (tombstone + hint).
	db2 := openCapped(t, dir, 3)
	if db2.Has("call:1") {
		t.Error("eviction of call:1 must survive reopen")
	}
	if got := mustBy(t, db2, "traceId", "T"); !eqStrs(got, []string{"call:2", "call:3", "call:4"}) {
		t.Errorf("after reopen traceId T = %v", got)
	}
	db2.Close()

	// Reopen with a LOWER cap: the group is over the cap at boot (no sweep),
	// and the next write to that value drains the whole excess.
	db3 := openCapped(t, dir, 1)
	defer db3.Close()
	if got := mustBy(t, db3, "traceId", "T"); len(got) != 3 {
		t.Fatalf("boot state = %v, want the 3 pre-existing records (no boot sweep)", got)
	}
	db3.PutIndexed("call:5", []byte(`{"v":"x"}`), IndexValues{"traceId": "T"})
	if got := mustBy(t, db3, "traceId", "T"); !eqStrs(got, []string{"call:5"}) {
		t.Errorf("after cap-lowered write traceId T = %v, want [call:5]", got)
	}
}

func TestSecondaryRebuildAfterReopen(t *testing.T) {
	dir := t.TempDir()
	db := openIndexed(t, dir)
	db.PutIndexed("call:1", []byte(`{"v":"a"}`), IndexValues{"traceId": "T1", "status": 200})
	db.PutIndexed("call:2", []byte(`{"v":"b"}`), IndexValues{"traceId": "T1", "status": 500})
	db.Close()

	// Reopen: secondary indexes must be rebuilt from the persisted ix fields,
	// with no extractors needed.
	db2 := openIndexed(t, dir)
	defer db2.Close()
	if got := mustBy(t, db2, "traceId", "T1"); !eqStrs(got, []string{"call:1", "call:2"}) {
		t.Errorf("after reopen traceId T1 = %v", got)
	}
	if got, _ := db2.ByIndexRange("status", 500, 599); !eqStrs(got, []string{"call:2"}) {
		t.Errorf("after reopen status 500-599 = %v", got)
	}
}

func TestSecondaryCompactionPreserves(t *testing.T) {
	db := openIndexed(t, t.TempDir())
	defer db.Close()

	db.PutIndexed("call:1", []byte(`{"v":"a"}`), IndexValues{"traceId": "T1"})
	db.PutIndexed("call:1", []byte(`{"v":"a2"}`), IndexValues{"traceId": "T1"}) // dead record
	db.PutIndexed("call:2", []byte(`{"v":"b"}`), IndexValues{"traceId": "T1"})

	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := mustBy(t, db, "traceId", "T1"); !eqStrs(got, []string{"call:1", "call:2"}) {
		t.Errorf("after compaction traceId T1 = %v", got)
	}
}

func TestSecondaryExtractor(t *testing.T) {
	// An index that derives its value from the record's JSON body.
	dir := t.TempDir()
	def := IndexDef{
		Name: "kind",
		Kind: KindString,
		Extract: func(key string, value []byte) (any, bool) {
			var m struct {
				Kind string `json:"kind"`
			}
			if json.Unmarshal(value, &m) != nil || m.Kind == "" {
				return nil, false
			}
			return m.Kind, true
		},
	}
	db, err := Open(Options{Dir: dir, Indexes: []IndexDef{def}})
	if err != nil {
		t.Fatal(err)
	}

	// Plain Put — the extractor derives "kind" automatically.
	db.Put("r1", []byte(`{"kind":"request"}`))
	db.Put("r2", []byte(`{"kind":"history"}`))
	db.Put("r3", []byte(`{"kind":"request"}`))

	if got := mustBy(t, db, "kind", "request"); !eqStrs(got, []string{"r1", "r3"}) {
		t.Errorf("kind=request = %v, want [r1 r3]", got)
	}
	db.Close()

	// Reopen WITHOUT the extractor: index still rebuilds from persisted ix.
	db2, err := Open(Options{Dir: dir, Indexes: []IndexDef{{Name: "kind", Kind: KindString}}})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if got := mustBy(t, db2, "kind", "history"); !eqStrs(got, []string{"r2"}) {
		t.Errorf("after reopen kind=history = %v, want [r2]", got)
	}
}

func TestSecondaryErrors(t *testing.T) {
	db := openIndexed(t, t.TempDir())
	defer db.Close()

	// Unknown index name.
	if err := db.PutIndexed("k", []byte(`{"v":"v"}`), IndexValues{"nope": "x"}); err == nil {
		t.Error("expected error for unknown index")
	}
	// Wrong value kind (string into number index).
	if err := db.PutIndexed("k", []byte(`{"v":"v"}`), IndexValues{"status": "not-a-number"}); err == nil {
		t.Error("expected error for wrong value kind")
	}
	// A failed PutIndexed must not have written anything.
	if db.Has("k") {
		t.Error("k must not exist after a rejected PutIndexed")
	}
	// Query on unknown index errors.
	if _, err := db.ByIndex("nope", "x"); err == nil {
		t.Error("expected error querying unknown index")
	}
}

func TestSecondaryDuplicateNameRejected(t *testing.T) {
	_, err := Open(Options{
		Dir:     t.TempDir(),
		Indexes: []IndexDef{{Name: "x", Kind: KindString}, {Name: "x", Kind: KindNumber}},
	})
	if err == nil {
		t.Error("expected error for duplicate index name")
	}
}

// Only JSON-object values may carry secondary index entries. Immediate values
// (strings, numbers, booleans, arrays, binary) are primary-key-only: Extract
// functions never run over them, and explicit index values are rejected.
func TestImmediateValuesArePKOnly(t *testing.T) {
	db, err := Open(Options{Dir: t.TempDir(), Indexes: []IndexDef{
		{Name: "traceId", Kind: KindString},
		{Name: "firstByte", Kind: KindString, Extract: func(key string, value []byte) (any, bool) {
			if len(value) == 0 {
				return nil, false
			}
			return string(value[:1]), true // permissive: would index ANY value shape
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Extract never runs over immediate values...
	immediates := map[string][]byte{
		"str":   []byte("hello"),
		"num":   []byte("42"),
		"float": []byte("1.5"),
		"bool":  []byte("true"),
		"arr":   []byte(`[1,2,3]`),
		"bin":   {0xff, 0xfe, 0x00},
	}
	for key, val := range immediates {
		if err := db.Put(key, val); err != nil {
			t.Fatalf("put %q: %v", key, err)
		}
	}
	for _, val := range []string{"h", "4", "1", "t", "[", "\xff"} {
		if keys := mustBy(t, db, "firstByte", val); len(keys) != 0 {
			t.Errorf("immediate value indexed by Extract at %q: %v", val, keys)
		}
	}
	// ...but does run over object values.
	if err := db.Put("doc", []byte(`{"kind":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if keys := mustBy(t, db, "firstByte", "{"); !eqStrs(keys, []string{"doc"}) {
		t.Errorf("object record not indexed: %v", keys)
	}
	// Leading whitespace does not hide an object from the shape check (the
	// extractor still sees the raw bytes, so it derives the space byte here).
	if err := db.Put("padded", []byte(`  {"kind":"y"}`)); err != nil {
		t.Fatal(err)
	}
	if keys := mustBy(t, db, "firstByte", " "); !eqStrs(keys, []string{"padded"}) {
		t.Errorf("whitespace-prefixed object skipped by Extract: %v", keys)
	}

	// Explicit index values on an immediate value are rejected, nothing written.
	if err := db.PutIndexed("k", []byte("scalar"), IndexValues{"traceId": "T"}); err == nil {
		t.Fatal("expected error: explicit index values on an immediate value")
	}
	if db.Has("k") {
		t.Error("k must not exist after rejected PutIndexed")
	}

	// A batch containing one offending item writes nothing.
	err = db.PutBatch([]PutItem{
		{Key: "ok", Value: []byte(`{"a":1}`), IX: IndexValues{"traceId": "T"}},
		{Key: "bad", Value: []byte("scalar"), IX: IndexValues{"traceId": "T"}},
	})
	if err == nil {
		t.Fatal("expected batch rejection")
	}
	if db.Has("ok") || db.Has("bad") {
		t.Error("rejected batch must write nothing")
	}

	// Explicit values on an object value still work (the feature is intact).
	if err := db.PutIndexed("call:1", []byte(`{"a":1}`), IndexValues{"traceId": "T"}); err != nil {
		t.Fatal(err)
	}
	if keys := mustBy(t, db, "traceId", "T"); !eqStrs(keys, []string{"call:1"}) {
		t.Errorf("traceId T = %v, want [call:1]", keys)
	}
}
