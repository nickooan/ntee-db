package nteedb

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		return 0
	}
	return bytes.Count(b, []byte{'\n'})
}

func TestCompactReclaimsDeadRecords(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)

	db.Put("a", []byte("1"))
	db.Put("b", []byte("2"))
	db.Put("a", []byte("11")) // supersedes a@1
	db.Put("c", []byte("3"))
	db.Delete("b") // tombstone

	// 5 appended records on disk; only {a, c} are live.
	if got := countLines(t, filepath.Join(dir, mainFile)); got != 5 {
		t.Fatalf("pre-compact line count = %d, want 5", got)
	}

	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}

	if got := countLines(t, filepath.Join(dir, mainFile)); got != 2 {
		t.Errorf("post-compact line count = %d, want 2 (a, c)", got)
	}

	// Data is unchanged by compaction.
	if v, ok := mustGet(t, db, "a"); !ok || v != "11" {
		t.Errorf("a = %q %v", v, ok)
	}
	if v, ok := mustGet(t, db, "c"); !ok || v != "3" {
		t.Errorf("c = %q %v", v, ok)
	}
	if _, ok := mustGet(t, db, "b"); ok {
		t.Error("b should remain deleted")
	}

	// Writes still work after compaction.
	if err := db.Put("d", []byte("4")); err != nil {
		t.Fatal(err)
	}
	if v, ok := mustGet(t, db, "d"); !ok || v != "4" {
		t.Errorf("d = %q %v", v, ok)
	}
	db.Close()

	// Reopen from the compacted files.
	db2 := mustOpen(t, dir)
	defer db2.Close()
	keys, _ := db2.PrefixScan("")
	if !eqStrs(keys, []string{"a", "c", "d"}) {
		t.Errorf("after reopen keys = %v, want [a c d]", keys)
	}
}

func TestCompactPreservesBlobValues(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, BlobThreshold: 32})
	if err != nil {
		t.Fatal(err)
	}

	big := bytes.Repeat([]byte("Q"), 4096)
	db.Put("blobby", big)
	db.Put("blobby", big) // overwrite (creates a dead earlier record + dead blob)
	db.Put("small", []byte("s"))

	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}

	// Blob ref copied verbatim; value still readable after compaction.
	if v, ok, _ := db.Get("blobby"); !ok || !bytes.Equal(v, big) {
		t.Error("blob value lost after compaction")
	}
	db.Close()

	db2, err := Open(Options{Dir: dir, BlobThreshold: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if v, ok, _ := db2.Get("blobby"); !ok || !bytes.Equal(v, big) {
		t.Error("blob value lost after compaction + reopen")
	}
}

func TestCompactEmptyDB(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	defer db.Close()
	if err := db.Compact(); err != nil {
		t.Fatalf("compact empty db: %v", err)
	}
	if got := countLines(t, filepath.Join(dir, mainFile)); got != 0 {
		t.Errorf("empty compacted log has %d lines, want 0", got)
	}
}
