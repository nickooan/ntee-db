package nteedb

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// blobOpts makes every value of 32+ bytes blob-backed.
func blobOpts(dir string) Options { return Options{Dir: dir, BlobThreshold: 32} }

func blobValue(fill byte) []byte { return bytes.Repeat([]byte{fill}, 100) }

func TestBlobUsage(t *testing.T) {
	db, err := Open(blobOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Blob-free store: zeros (one empty generation file exists).
	u, err := db.BlobUsage()
	if err != nil {
		t.Fatal(err)
	}
	if u.TotalBytes != 0 || u.LiveBytes != 0 || u.OrphanedBytes != 0 || u.Generations != 1 {
		t.Fatalf("fresh store usage: %+v", u)
	}

	// 4 blobs written; overwrite one, delete one → 2 live, 5 on disk.
	for i := 0; i < 4; i++ {
		db.Put(fmt.Sprintf("k%d", i), blobValue(byte('a'+i)))
	}
	db.Put("k0", blobValue('x'))
	db.Delete("k1")
	// An inline value must not count toward blob usage.
	db.Put("small", []byte("1"))

	u, err = db.BlobUsage()
	if err != nil {
		t.Fatal(err)
	}
	if u.TotalBytes != 500 || u.LiveBytes != 300 || u.OrphanedBytes != 200 {
		t.Fatalf("usage after churn: %+v (want 500/300/200)", u)
	}
}

func TestBlobsRelieveReclaimsOrphans(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(blobOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 4; i++ {
		if err := db.Put(fmt.Sprintf("k%d", i), blobValue(byte('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	db.Put("k0", blobValue('x'))
	db.Put("k1", blobValue('y'))
	db.Delete("k2")

	if err := db.BlobsRelieve(); err != nil {
		t.Fatal(err)
	}
	u, err := db.BlobUsage()
	if err != nil {
		t.Fatal(err)
	}
	if u.TotalBytes != 300 || u.OrphanedBytes != 0 || u.Generations != 1 {
		t.Fatalf("post-relieve usage: %+v (want 300 total, 0 orphaned, 1 gen)", u)
	}
	// Old generation gone, new one present.
	if _, err := os.Stat(filepath.Join(dir, "blobs.dat")); !os.IsNotExist(err) {
		t.Error("gen-0 blobs.dat should have been retired")
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs.1.dat")); err != nil {
		t.Errorf("blobs.1.dat missing: %v", err)
	}
	// Values readable from the new generation, before and after reopen.
	verify := func(db *DB, stage string) {
		t.Helper()
		for key, fill := range map[string]byte{"k0": 'x', "k1": 'y', "k3": 'a' + 3} {
			v, ok, err := db.Get(key)
			if err != nil || !ok || !bytes.Equal(v, blobValue(fill)) {
				t.Fatalf("%s: bad %s: ok=%v err=%v", stage, key, ok, err)
			}
		}
	}
	verify(db, "post-relieve")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(blobOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	verify(db2, "reopen")
	// New writes append to the new generation.
	if err := db2.Put("k9", blobValue('z')); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := db2.Get("k9"); err != nil || !ok || !bytes.Equal(v, blobValue('z')) {
		t.Fatalf("write after relieve+reopen: ok=%v err=%v", ok, err)
	}
}

func TestBlobsRelieveConsolidatesStrayGeneration(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(blobOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	db.Put("k1", blobValue('a'))
	db.Close()

	// Simulate a BlobsRelieve that crashed before the main-log rename: a stray
	// half-written next-generation file (and a stale .compact main).
	if err := os.WriteFile(filepath.Join(dir, "blobs.1.dat"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, mainFile+".compact"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err = Open(blobOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Old refs (gen 0) still resolve even though appends now target gen 1.
	if v, ok, err := db.Get("k1"); err != nil || !ok || !bytes.Equal(v, blobValue('a')) {
		t.Fatalf("gen-0 ref unreadable after crash recovery: ok=%v err=%v", ok, err)
	}
	if db.curGen != 1 {
		t.Fatalf("appends should target the highest gen, got %d", db.curGen)
	}
	// The measurement exposes the stray so a caller's policy can react.
	u, err := db.BlobUsage()
	if err != nil {
		t.Fatal(err)
	}
	if u.Generations != 2 {
		t.Fatalf("usage should report 2 generations: %+v", u)
	}

	if err := db.BlobsRelieve(); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"blobs.dat", "blobs.1.dat"} {
		if _, err := os.Stat(filepath.Join(dir, stale)); !os.IsNotExist(err) {
			t.Errorf("%s should have been retired", stale)
		}
	}
	if u, _ := db.BlobUsage(); u.Generations != 1 || u.OrphanedBytes != 0 {
		t.Fatalf("post-consolidation usage: %+v", u)
	}
	if v, ok, err := db.Get("k1"); err != nil || !ok || !bytes.Equal(v, blobValue('a')) {
		t.Fatalf("value lost in consolidation: ok=%v err=%v", ok, err)
	}
}

func TestCompactAndReindexPreserveGenerations(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(blobOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.Put("k1", blobValue('a'))
	db.Put("k1", blobValue('b')) // orphan the first version
	if err := db.BlobsRelieve(); err != nil {
		t.Fatal(err)
	}
	// Now on gen 1. Plain Compact and Reindex must keep refs valid (gen 1).
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := db.Reindex(); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := db.Get("k1"); err != nil || !ok || !bytes.Equal(v, blobValue('b')) {
		t.Fatalf("gen-1 ref broken by Compact/Reindex: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs.1.dat")); err != nil {
		t.Errorf("blobs.1.dat should survive Compact/Reindex: %v", err)
	}
}

func TestDestroyRemovesAllGenerations(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(blobOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	db.Put("k1", blobValue('a'))
	db.Put("k1", blobValue('b'))
	if err := db.BlobsRelieve(); err != nil {
		t.Fatal(err)
	}
	if err := db.Drop(); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "blobs*"))
	if len(left) != 0 {
		t.Fatalf("blob files left after Drop: %v", left)
	}
}
