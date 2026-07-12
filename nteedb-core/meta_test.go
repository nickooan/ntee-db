package nteedb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func kindIndex(name string) IndexDef { return IndexDef{Name: name, Kind: KindString} }

func TestMetaWrittenAndReflectsSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, Indexes: []IndexDef{
		{Name: "traceId", Kind: KindString},
		{Name: "status", Kind: KindNumber},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	m, ok := loadMeta(filepath.Join(dir, metaFile))
	if !ok {
		t.Fatal("meta.json not written")
	}
	if len(m.Indexes) != 2 {
		t.Fatalf("meta has %d indexes, want 2", len(m.Indexes))
	}
	byName := map[string]metaIndex{}
	for _, mi := range m.Indexes {
		byName[mi.Name] = mi
	}
	if byName["traceId"].Kind != "string" || byName["status"].Kind != "number" {
		t.Errorf("kinds wrong: %+v", m.Indexes)
	}
	// Fresh store has no pre-existing data, so indexes are complete (not prospective).
	if !byName["traceId"].Complete {
		t.Error("index on fresh store should be complete")
	}
	if len(db.ProspectiveIndexes()) != 0 {
		t.Errorf("fresh store should have no prospective indexes, got %v", db.ProspectiveIndexes())
	}
}

func TestProspectiveOnAddingIndexToExistingData(t *testing.T) {
	dir := t.TempDir()

	// Store created with one index and some data.
	db := mustOpenIdx(t, dir, kindIndex("traceId"))
	db.PutIndexed("call:1", []byte(`{"v":"a"}`), IndexValues{"traceId": "T1"})
	db.Close()

	// Reopen adding a new index — it's prospective (old records don't have it).
	db2, err := Open(Options{Dir: dir, Indexes: []IndexDef{
		kindIndex("traceId"),
		kindIndex("session"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if got := db2.ProspectiveIndexes(); !eqStrs(got, []string{"session"}) {
		t.Errorf("prospective = %v, want [session]", got)
	}
	// The old record is not covered by the new index.
	if res, _ := db2.ByIndex("session", "anything"); len(res) != 0 {
		t.Errorf("new index should not cover old records, got %v", res)
	}
	// But it covers new writes.
	db2.PutIndexed("call:2", []byte(`{"v":"b"}`), IndexValues{"traceId": "T1", "session": "S1"})
	if res, _ := db2.ByIndex("session", "S1"); !eqStrs(res, []string{"call:2"}) {
		t.Errorf("new index should cover new writes, got %v", res)
	}
}

func mustOpenIdx(t *testing.T, dir string, defs ...IndexDef) *DB {
	t.Helper()
	db, err := Open(Options{Dir: dir, Indexes: defs})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

func TestProspectivePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	db := mustOpenIdx(t, dir, kindIndex("traceId"))
	db.PutIndexed("call:1", []byte(`{"v":"a"}`), IndexValues{"traceId": "T1"})
	db.Close()

	// Add an index (prospective), then close WITHOUT reindexing.
	db2 := mustOpenIdx(t, dir, kindIndex("traceId"), kindIndex("session"))
	if !eqStrs(db2.ProspectiveIndexes(), []string{"session"}) {
		t.Fatalf("session should be prospective")
	}
	db2.Close()

	// Reopen again: the prospective status must persist (meta recorded incomplete).
	db3 := mustOpenIdx(t, dir, kindIndex("traceId"), kindIndex("session"))
	defer db3.Close()
	if !eqStrs(db3.ProspectiveIndexes(), []string{"session"}) {
		t.Errorf("session should still be prospective after reopen, got %v", db3.ProspectiveIndexes())
	}
}

func TestReindexBackfillsExtractIndex(t *testing.T) {
	dir := t.TempDir()

	// Records written before the "kind" index exists.
	db := mustOpenIdx(t, dir) // no indexes
	db.Put("r1", []byte(`{"kind":"request"}`))
	db.Put("r2", []byte(`{"kind":"history"}`))
	db.Put("r3", []byte(`{"kind":"request"}`))
	db.Close()

	// Reopen declaring an Extract-based index — prospective at first.
	kindDef := IndexDef{
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
	db2, err := Open(Options{Dir: dir, Indexes: []IndexDef{kindDef}})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if !eqStrs(db2.ProspectiveIndexes(), []string{"kind"}) {
		t.Fatalf("kind should be prospective before reindex, got %v", db2.ProspectiveIndexes())
	}
	if res, _ := db2.ByIndex("kind", "request"); len(res) != 0 {
		t.Errorf("before reindex, old records uncovered; got %v", res)
	}

	// Back-fill.
	if err := db2.Reindex(); err != nil {
		t.Fatal(err)
	}
	if len(db2.ProspectiveIndexes()) != 0 {
		t.Errorf("after reindex, no prospective indexes; got %v", db2.ProspectiveIndexes())
	}
	if res, _ := db2.ByIndex("kind", "request"); !eqStrs(res, []string{"r1", "r3"}) {
		t.Errorf("after reindex kind=request = %v, want [r1 r3]", res)
	}
	if res, _ := db2.ByIndex("kind", "history"); !eqStrs(res, []string{"r2"}) {
		t.Errorf("after reindex kind=history = %v, want [r2]", res)
	}
}

func TestReindexBackfillsBlobBackedRecord(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, BlobThreshold: 16}) // force blobs, no index yet
	if err != nil {
		t.Fatal(err)
	}
	// A JSON body padded past the 16-byte blob threshold so it lands in blobs.dat.
	big := []byte(`{"kind":"request","pad":"` + strings.Repeat("x", 64) + `"}`)
	db.Put("r1", big)
	db.Close()

	def := IndexDef{Name: "kind", Kind: KindString, Extract: func(key string, value []byte) (any, bool) {
		var m struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(value, &m) != nil || m.Kind == "" {
			return nil, false
		}
		return m.Kind, true
	}}
	db2, err := Open(Options{Dir: dir, BlobThreshold: 16, Indexes: []IndexDef{def}})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := db2.Reindex(); err != nil { // must read the blob value to derive the index
		t.Fatal(err)
	}
	if res, _ := db2.ByIndex("kind", "request"); !eqStrs(res, []string{"r1"}) {
		t.Errorf("reindex over blob-backed record = %v, want [r1]", res)
	}
}

func TestSoftDropCompactPreservesReindexPurges(t *testing.T) {
	dir := t.TempDir()
	db := mustOpenIdx(t, dir, kindIndex("traceId"))
	db.PutIndexed("call:1", []byte(`{"v":"a"}`), IndexValues{"traceId": "T1"})
	db.Close()

	// Reopen WITHOUT traceId — it's soft-dropped, not removed.
	db2 := mustOpenIdx(t, dir)

	if !eqStrs(db2.DroppedIndexes(), []string{"traceId"}) {
		t.Errorf("DroppedIndexes = %v, want [traceId]", db2.DroppedIndexes())
	}
	// Querying a dropped index still errors (it's not active/usable).
	if _, err := db2.ByIndex("traceId", "T1"); err == nil {
		t.Error("querying dropped index should error")
	}
	// meta keeps the tombstone.
	if m, _ := loadMeta(filepath.Join(dir, metaFile)); !hasDroppedMeta(m, "traceId") {
		t.Error("meta should keep traceId as a dropped tombstone")
	}

	// Compact PRESERVES the dropped field (soft-drop).
	if err := db2.Compact(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, mainFile))
	if !strings.Contains(string(b), "traceId") {
		t.Errorf("Compact should preserve soft-dropped field; main.jsonl = %s", b)
	}

	// Reindex PURGES it from both records and meta.
	if err := db2.Reindex(); err != nil {
		t.Fatal(err)
	}
	if len(db2.DroppedIndexes()) != 0 {
		t.Errorf("after Reindex, DroppedIndexes = %v, want empty", db2.DroppedIndexes())
	}
	b, _ = os.ReadFile(filepath.Join(dir, mainFile))
	if strings.Contains(string(b), "traceId") {
		t.Errorf("Reindex should purge dropped field; main.jsonl = %s", b)
	}
	if m, _ := loadMeta(filepath.Join(dir, metaFile)); hasDroppedMeta(m, "traceId") {
		t.Error("Reindex should remove the dropped tombstone from meta")
	}
	db2.Close()
}

func hasDroppedMeta(m metaData, name string) bool {
	for _, mi := range m.Indexes {
		if mi.Name == name && mi.Dropped {
			return true
		}
	}
	return false
}

func TestSoftDropReaddRecoversData(t *testing.T) {
	dir := t.TempDir()
	db := mustOpenIdx(t, dir, kindIndex("traceId"))
	db.PutIndexed("call:1", []byte(`{"v":"a"}`), IndexValues{"traceId": "T1"})
	db.Close()

	// Drop it (soft), no compaction between.
	db2 := mustOpenIdx(t, dir)
	if !eqStrs(db2.DroppedIndexes(), []string{"traceId"}) {
		t.Fatal("traceId should be soft-dropped")
	}
	db2.Close()

	// Re-add it: surviving ix data repopulates the index on boot.
	db3 := mustOpenIdx(t, dir, kindIndex("traceId"))
	defer db3.Close()
	if len(db3.DroppedIndexes()) != 0 {
		t.Errorf("re-added index should no longer be dropped, got %v", db3.DroppedIndexes())
	}
	if res, _ := db3.ByIndex("traceId", "T1"); !eqStrs(res, []string{"call:1"}) {
		t.Errorf("re-added index should recover surviving data; got %v", res)
	}
}

func TestDestroyAndDrop(t *testing.T) {
	dir := t.TempDir()
	db := mustOpenIdx(t, dir, kindIndex("traceId"))
	db.PutIndexed("call:1", []byte(`{"v":"a"}`), IndexValues{"traceId": "T1"})

	if err := db.Drop(); err != nil {
		t.Fatal(err)
	}
	// All store files must be gone.
	for _, name := range storeFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should not exist after Drop", name)
		}
	}
	// Reopen creates a fresh, empty store.
	db2 := mustOpenIdx(t, dir, kindIndex("traceId"))
	defer db2.Close()
	if db2.Has("call:1") {
		t.Error("store should be empty after Drop")
	}

	// Destroy is safe to call again (idempotent / missing files ignored).
	db2.Close()
	if err := Destroy(dir); err != nil {
		t.Fatalf("Destroy on already-destroyed store: %v", err)
	}
}
