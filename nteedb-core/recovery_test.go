package nteedb

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestCrashRecoveryTornTail simulates a crash: data reaches main.jsonl but no
// hint is written, and the final record is torn. Reopen must recover the durable
// records, hide the torn one, truncate it, and accept new writes cleanly.
func TestCrashRecoveryTornTail(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	if err := db.Put("a", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("b", []byte("2")); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash: flush + drop handles WITHOUT writing a hint. The lock
	// release mirrors what the kernel does on process death.
	db.main.flush()
	db.main.close()
	db.rf.Close()
	for _, bs := range db.blobs {
		bs.close()
	}
	db.lock.Close()
	db.closed = true

	// A crash mid-append leaves a partial line with no trailing newline.
	f, err := os.OpenFile(filepath.Join(dir, mainFile), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"k":"c","v":`)
	f.Close()

	// No hint → full scan; the torn tail must be self-healed.
	db2 := mustOpen(t, dir)
	defer db2.Close()
	if v, ok := mustGet(t, db2, "a"); !ok || v != "1" {
		t.Errorf("a = %q %v", v, ok)
	}
	if v, ok := mustGet(t, db2, "b"); !ok || v != "2" {
		t.Errorf("b = %q %v", v, ok)
	}
	if db2.Has("c") {
		t.Error("torn record c must not be visible")
	}

	// The torn tail must be truncated so subsequent writes land cleanly.
	if err := db2.Put("c", []byte("3")); err != nil {
		t.Fatal(err)
	}
	if v, ok := mustGet(t, db2, "c"); !ok || v != "3" {
		t.Errorf("c = %q %v after recovery write", v, ok)
	}
}

// TestConcurrentReadWrite exercises the RWMutex under -race.
func TestConcurrentReadWrite(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	for i := 0; i < 50; i++ {
		if err := db.Put(fmt.Sprintf("k%03d", i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = db.Put(fmt.Sprintf("w%d-%03d", w, i), []byte("x"))
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, _, _ = db.Get(fmt.Sprintf("k%03d", i%50))
				_, _ = db.PrefixScan("k0")
				_ = db.Has("k001")
			}
		}()
	}
	wg.Wait()

	// All writes must be present.
	for w := 0; w < 4; w++ {
		for i := 0; i < 100; i++ {
			if !db.Has(fmt.Sprintf("w%d-%03d", w, i)) {
				t.Fatalf("missing w%d-%03d", w, i)
			}
		}
	}
}

// TestConcurrentCompact runs compaction concurrently with readers.
func TestConcurrentCompact(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()
	for i := 0; i < 100; i++ {
		db.Put(fmt.Sprintf("k%03d", i), []byte("v"))
		db.Put(fmt.Sprintf("k%03d", i), []byte("v2")) // create dead records
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_, _, _ = db.Get(fmt.Sprintf("k%03d", i%100))
		}
	}()
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if v, ok := mustGet(t, db, "k050"); !ok || v != "v2" {
		t.Errorf("k050 = %q %v after concurrent compact", v, ok)
	}
}
