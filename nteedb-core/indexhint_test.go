package nteedb

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestHintFastPathBoot(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	db.Put("a", []byte("1"))
	db.Put("b", []byte("2"))
	db.Close() // writes a hint covering everything

	// A hint file must now exist.
	if _, err := os.Stat(filepath.Join(dir, hintFile)); err != nil {
		t.Fatalf("hint file missing after Close: %v", err)
	}

	db2 := mustOpen(t, dir)
	defer db2.Close()
	if v, ok := mustGet(t, db2, "a"); !ok || v != "1" {
		t.Errorf("a = %q %v", v, ok)
	}
	if v, ok := mustGet(t, db2, "b"); !ok || v != "2" {
		t.Errorf("b = %q %v", v, ok)
	}
}

func TestHintStaleThenTailReplay(t *testing.T) {
	dir := t.TempDir()

	// Write a hint covering only {a}, then append more records WITHOUT updating
	// the hint, to simulate writes after the last checkpoint.
	db := mustOpen(t, dir)
	db.Put("a", []byte("1"))
	if err := db.writeHintLocked(); err != nil { // checkpoint at {a}
		t.Fatal(err)
	}
	db.Put("b", []byte("2"))
	db.Put("a", []byte("11")) // overwrite after checkpoint
	// Close writes a fresh full hint, so to keep the hint stale we bypass it.
	// Releasing the lock mirrors what the kernel does on process death.
	db.main.flush()
	db.main.close()
	db.rf.Close()
	db.lock.Close()
	db.closed = true

	// Reopen: hint covers only {a}@old; boot must replay the tail to pick up
	// {b} and the {a} overwrite.
	db2 := mustOpen(t, dir)
	defer db2.Close()
	if v, ok := mustGet(t, db2, "a"); !ok || v != "11" {
		t.Errorf("a = %q %v, want 11 (tail overwrite applied)", v, ok)
	}
	if v, ok := mustGet(t, db2, "b"); !ok || v != "2" {
		t.Errorf("b = %q %v, want 2 (tail record applied)", v, ok)
	}
}

func TestCorruptHintFallsBackToFullScan(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	db.Put("a", []byte("1"))
	db.Put("b", []byte("2"))
	db.Close()

	// Corrupt the hint file.
	if err := os.WriteFile(filepath.Join(dir, hintFile), []byte("not json at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	db2 := mustOpen(t, dir) // must full-scan and still be correct
	defer db2.Close()
	if v, ok := mustGet(t, db2, "a"); !ok || v != "1" {
		t.Errorf("a = %q %v", v, ok)
	}
	if v, ok := mustGet(t, db2, "b"); !ok || v != "2" {
		t.Errorf("b = %q %v", v, ok)
	}
}

func TestHintAheadOfLogIgnored(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	db.Put("a", []byte("1"))
	db.Close()

	// Forge a hint whose covers watermark is past the end of the log.
	ix := newPkIndex()
	ix.upsert(pkEntry{key: "ghost", off: 0, n: 5})
	if err := writeIndexHint(filepath.Join(dir, hintFile), ix, 1<<30); err != nil {
		t.Fatal(err)
	}

	db2 := mustOpen(t, dir) // covers > size → ignore hint, full-scan
	defer db2.Close()
	if db2.Has("ghost") {
		t.Error("ghost key from bogus hint should not be present")
	}
	if v, ok := mustGet(t, db2, "a"); !ok || v != "1" {
		t.Errorf("a = %q %v", v, ok)
	}
}

func TestPeriodicHintEveryN(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, HintEveryN: 2})
	if err != nil {
		t.Fatal(err)
	}
	db.Put("a", []byte("1")) // write 1
	db.Put("b", []byte("2")) // write 2 → background hint rewrite triggered here
	hintPath := filepath.Join(dir, hintFile)

	// The periodic hint is written by a background goroutine — poll for it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, covers, ok := loadIndexHint(hintPath)
		if ok {
			if len(entries) != 2 || covers <= 0 {
				t.Errorf("hint entries=%d covers=%d, want 2 entries and positive covers", len(entries), covers)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected a hint to appear after HintEveryN writes")
		}
		time.Sleep(5 * time.Millisecond)
	}
	db.Close()
}

// TestAsyncHintUnderConcurrentWrites hammers the write path with background
// hint rewrites firing constantly, from several goroutines, while readers run.
// Its main value is under `go test -race`.
func TestAsyncHintUnderConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{
		Dir:        dir,
		HintEveryN: 1, // trigger a background hint on every write
		Indexes:    []IndexDef{{Name: "traceId", Kind: KindString}},
	})
	if err != nil {
		t.Fatal(err)
	}

	const writers, perWriter = 4, 100
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				key := fmt.Sprintf("k%d-%03d", w, i)
				if err := db.PutIndexed(key, []byte("v"), IndexValues{"traceId": fmt.Sprintf("T%d", w)}); err != nil {
					t.Errorf("put %s: %v", key, err)
				}
				db.Get(key)
				db.ByIndex("traceId", fmt.Sprintf("T%d", w), -1)
			}
		}(w)
	}
	wg.Wait()
	db.Close()

	// Reopen: every record must be present regardless of hint timing.
	db2 := mustOpen(t, dir)
	defer db2.Close()
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			if !db2.Has(fmt.Sprintf("k%d-%03d", w, i)) {
				t.Fatalf("k%d-%03d missing after reopen", w, i)
			}
		}
	}
}

// TestCloseWaitsForInflightHint closes immediately after triggering a
// background hint: Close must wait it out, leave a valid hint, and a reopen
// must fast-boot to the full state.
func TestCloseWaitsForInflightHint(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, HintEveryN: 1})
	if err != nil {
		t.Fatal(err)
	}
	db.Put("a", []byte("1")) // triggers a background hint
	db.Put("b", []byte("2")) // may coalesce into the next
	db.Close()               // must wait for the in-flight writer, then checkpoint

	entries, covers, ok := loadIndexHint(filepath.Join(dir, hintFile))
	if !ok {
		t.Fatal("hint missing or unparseable after Close")
	}
	if len(entries) != 2 || covers <= 0 {
		t.Errorf("hint entries=%d covers=%d, want 2 and positive", len(entries), covers)
	}

	db2 := mustOpen(t, dir)
	defer db2.Close()
	if v, ok := mustGet(t, db2, "a"); !ok || v != "1" {
		t.Errorf("a = %q %v", v, ok)
	}
	if v, ok := mustGet(t, db2, "b"); !ok || v != "2" {
		t.Errorf("b = %q %v", v, ok)
	}
}

// TestCompactSupersedesAsyncHint runs Compact right after triggering a
// background hint. Whether the async snapshot lands first (then overwritten)
// or is gen-skipped, the surviving hint and a reopen must reflect the
// compacted state.
func TestCompactSupersedesAsyncHint(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, HintEveryN: 1})
	if err != nil {
		t.Fatal(err)
	}
	db.Put("a", []byte("1"))
	db.Put("a", []byte("2")) // dead version for compact to drop; triggers hint
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2 := mustOpen(t, dir)
	defer db2.Close()
	if v, ok := mustGet(t, db2, "a"); !ok || v != "2" {
		t.Errorf("a = %q %v, want 2", v, ok)
	}
}
