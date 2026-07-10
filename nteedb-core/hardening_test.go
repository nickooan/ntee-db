package nteedb

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A write whose key would be its own eviction victim under maxPerValue must be
// rejected up front — never silently tombstoned after a successful Put.
func TestSelfEvictionRejected(t *testing.T) {
	db, err := Open(Options{
		Dir:     t.TempDir(),
		Indexes: []IndexDef{{Name: "ep", Kind: KindString, MaxPerValue: 2}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.PutIndexed("k:3", []byte("a"), IndexValues{"ep": "E"}))
	must(db.PutIndexed("k:4", []byte("b"), IndexValues{"ep": "E"}))

	// Group {k:3, k:4} is full; a lower key would evict itself → rejected.
	err = db.PutIndexed("k:2", []byte("x"), IndexValues{"ep": "E"})
	if err == nil || !strings.Contains(err.Error(), "immediately evicted") {
		t.Fatalf("out-of-order write should be rejected, got %v", err)
	}
	// Nothing was written and nothing was lost.
	if _, ok := mustGet(t, db, "k:2"); ok {
		t.Error("rejected key must not exist")
	}
	if got := mustBy(t, db, "ep", "E"); !eqStrs(got, []string{"k:3", "k:4"}) {
		t.Errorf("group after rejection = %v, want [k:3 k:4]", got)
	}

	// Overwriting an existing member never grows the group → always allowed.
	must(db.PutIndexed("k:3", []byte("a2"), IndexValues{"ep": "E"}))
	// A higher key is normal retention: allowed, evicts the oldest.
	must(db.PutIndexed("k:5", []byte("c"), IndexValues{"ep": "E"}))
	if got := mustBy(t, db, "ep", "E"); !eqStrs(got, []string{"k:4", "k:5"}) {
		t.Errorf("group after k:5 = %v, want [k:4 k:5]", got)
	}

	// PutBatch validates the same rule up front: nothing is written.
	err = db.PutBatch([]PutItem{{Key: "k:1", Value: []byte("x"), IX: IndexValues{"ep": "E"}}})
	if err == nil || !strings.Contains(err.Error(), "immediately evicted") {
		t.Fatalf("batch with self-evicting item should be rejected, got %v", err)
	}
	if db.Has("k:1") {
		t.Error("rejected batch item must not exist")
	}
}

// A reopen failure during the compaction swap must fail-stop the store (every
// later call returns ErrClosed) instead of leaving it wedged on closed files —
// and the store must reopen cleanly afterwards.
func TestCompactFailStop(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	if err := db.Put("a", []byte("1")); err != nil {
		t.Fatal(err)
	}

	orig := openMainLogFn
	openMainLogFn = func(path string, sync bool) (*mainLog, error) {
		return nil, errors.New("injected reopen failure")
	}
	err := db.Compact()
	openMainLogFn = orig
	if err == nil || !strings.Contains(err.Error(), "disabled after failed compaction swap") {
		t.Fatalf("Compact should fail-stop, got %v", err)
	}

	// The store is disabled, not wedged: uniform ErrClosed, idempotent Close.
	if _, _, err := db.Get("a"); !errors.Is(err, ErrClosed) {
		t.Errorf("Get after fail-stop = %v, want ErrClosed", err)
	}
	if err := db.Put("b", []byte("2")); !errors.Is(err, ErrClosed) {
		t.Errorf("Put after fail-stop = %v, want ErrClosed", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("Close after fail-stop = %v, want nil", err)
	}

	// The flock was released and the swapped file is intact → clean reopen.
	db2 := mustOpen(t, dir)
	defer db2.Close()
	if v, ok := mustGet(t, db2, "a"); !ok || v != "1" {
		t.Errorf("after reopen a = %q %v, want \"1\" true", v, ok)
	}
}

// A main record whose blob ref points past the end of blobs.dat (power loss
// persisted the log page but not the blob page) is treated as the start of the
// torn tail: the record and everything after it are dropped, and the store
// recovers cleanly.
func TestCrashRecoveryDanglingBlobRef(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, BlobThreshold: 32})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Put("small", []byte("inline")); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte{0xab}, 4096)
	if err := db.Put("blob1", big); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("after", []byte("later")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate the power loss: blob bytes gone, main log fully persisted. The
	// hint must go too — a real power loss cannot produce a hint covering the
	// lost region (writeHint flushes blobs first), so force the full rescan.
	if err := os.Truncate(filepath.Join(dir, blobFile), 10); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, hintFile)); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(Options{Dir: dir, BlobThreshold: 32})
	if err != nil {
		t.Fatalf("reopen after blob loss: %v", err)
	}
	defer db2.Close()

	// Everything before the dangling ref survives; the ref and later writes are
	// the lost tail.
	if v, ok := mustGet(t, db2, "small"); !ok || v != "inline" {
		t.Errorf("small = %q %v, want \"inline\" true", v, ok)
	}
	for _, k := range []string{"blob1", "after"} {
		if _, ok := mustGet(t, db2, k); ok {
			t.Errorf("%s should be part of the lost tail", k)
		}
	}
	// The store is fully usable — new writes work.
	if err := db2.Put("new", []byte("x")); err != nil {
		t.Fatalf("write after recovery: %v", err)
	}
}

// ByIndexRange boundary semantics: inclusive [lo, hi], lo==hi, and lo>hi.
func TestByIndexRangeEdges(t *testing.T) {
	db, err := Open(Options{Dir: t.TempDir(), Indexes: []IndexDef{{Name: "n", Kind: KindNumber}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for i, v := range []int{100, 200, 300} {
		if err := db.PutIndexed(fmt.Sprintf("k%d", i), []byte("{}"), IndexValues{"n": v}); err != nil {
			t.Fatal(err)
		}
	}

	check := func(lo, hi any, want []string) {
		t.Helper()
		got, err := db.ByIndexRange("n", lo, hi)
		if err != nil {
			t.Fatalf("ByIndexRange(%v,%v): %v", lo, hi, err)
		}
		if !eqStrs(got, want) {
			t.Errorf("ByIndexRange(%v,%v) = %v, want %v", lo, hi, got, want)
		}
	}
	check(100, 300, []string{"k0", "k1", "k2"}) // both ends inclusive
	check(200, 200, []string{"k1"})             // lo == hi
	check(101, 199, []string{})                 // empty interior
	check(300, 100, []string{})                 // lo > hi → empty, not an error
	check(250, 300, []string{"k2"})             // value exactly at hi included
}

// Reads pread outside db.mu (retrying on a Compact swap): concurrent Get and
// GetMany must always return the correct value while Compact repeatedly rewrites
// and swaps the main log. Run under -race.
func TestGetDuringCompact(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	const n = 2000
	want := make(map[string]string, n)
	key := func(i int) string { return fmt.Sprintf("k%05d", i) }
	for i := 0; i < n; i++ {
		v := fmt.Sprintf("value-for-record-%05d", i)
		want[key(i)] = v
		if err := db.Put(key(i), []byte(v)); err != nil {
			t.Fatal(err)
		}
	}

	// Compact in a tight loop — the worst case for read/write contention.
	stop := make(chan struct{})
	var compactWG sync.WaitGroup
	compactWG.Add(1)
	go func() {
		defer compactWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if err := db.Compact(); err != nil {
					t.Errorf("compact: %v", err)
					return
				}
			}
		}
	}()

	var readers sync.WaitGroup
	errCh := make(chan error, 12)
	for r := 0; r < 12; r++ {
		readers.Add(1)
		go func(seed int) {
			defer readers.Done()
			for j := 0; j < 4000; j++ {
				if seed%2 == 0 { // half exercise Get
					k := key((seed*7 + j) % n)
					v, ok, err := db.Get(k)
					if err != nil {
						errCh <- fmt.Errorf("Get(%s): %w", k, err)
						return
					}
					if !ok || string(v) != want[k] {
						errCh <- fmt.Errorf("Get(%s) = %q %v, want %q", k, v, ok, want[k])
						return
					}
				} else { // the other half exercise GetMany
					ks := []string{key(j % n), key((j + 1) % n), "absent"}
					vals, found, err := db.GetMany(ks)
					if err != nil {
						errCh <- fmt.Errorf("GetMany: %w", err)
						return
					}
					for i := 0; i < 2; i++ {
						if !found[i] || string(vals[i]) != want[ks[i]] {
							errCh <- fmt.Errorf("GetMany[%d]=%q %v, want %q", i, vals[i], found[i], want[ks[i]])
							return
						}
					}
					if found[2] {
						errCh <- fmt.Errorf("GetMany absent key reported found")
						return
					}
				}
			}
		}(r)
	}

	readers.Wait()
	close(stop)
	compactWG.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// The async periodic-hint writer racing writes, Compact, and Close — the
// subtlest concurrency in the core. Run under -race; afterwards every record
// must survive a reopen.
func TestAsyncHintRaceWithCompact(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, HintEveryN: 1}) // hint attempt on every write
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const n = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if err := db.Put(fmt.Sprintf("k%03d", i), []byte("v")); err != nil {
				t.Errorf("put: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := db.Compact(); err != nil {
				t.Errorf("compact: %v", err)
				return
			}
		}
	}()
	wg.Wait()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2 := mustOpen(t, dir)
	defer db2.Close()
	for i := 0; i < n; i++ {
		if !db2.Has(fmt.Sprintf("k%03d", i)) {
			t.Fatalf("k%03d lost across hint/compact race", i)
		}
	}
}

// Stats reports live records and file sizes from in-memory counters.
func TestStats(t *testing.T) {
	db, err := Open(Options{Dir: t.TempDir(), BlobThreshold: 32})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if s := db.Stats(); s.Records != 0 || s.MainBytes != 0 || s.BlobBytes != 0 {
		t.Errorf("empty store stats = %+v, want zeros", s)
	}
	if err := db.Put("a", []byte("inline")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("b", bytes.Repeat([]byte{0xcd}, 100)); err != nil { // blob path
		t.Fatal(err)
	}
	s := db.Stats()
	if s.Records != 2 {
		t.Errorf("records = %d, want 2", s.Records)
	}
	if s.MainBytes <= 0 || s.BlobBytes != 100 {
		t.Errorf("sizes = main %d blob %d, want main > 0, blob 100", s.MainBytes, s.BlobBytes)
	}
	// A delete keeps the tombstone in main (until Compact) but drops the record.
	if err := db.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if s2 := db.Stats(); s2.Records != 1 || s2.MainBytes <= s.MainBytes {
		t.Errorf("after delete: %+v (main should grow by the tombstone)", s2)
	}
}

// Reindex drops an explicit index value whose declared kind changed — the old
// wrong-kind value must not be rewritten into records forever.
func TestReindexDropsWrongKindExplicitIX(t *testing.T) {
	dir := t.TempDir()
	db1, err := Open(Options{Dir: dir, Indexes: []IndexDef{{Name: "s", Kind: KindString}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db1.PutIndexed("k1", []byte("{}"), IndexValues{"s": "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen with the index's kind flipped string→number, then Reindex.
	db2, err := Open(Options{Dir: dir, Indexes: []IndexDef{{Name: "s", Kind: KindNumber}}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if err := db2.Reindex(); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	// The stale string value must be gone from the rewritten record.
	e, ok := db2.pk.get("k1")
	if !ok {
		t.Fatal("k1 missing after reindex")
	}
	rec, err := db2.readRecord(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := rec.IX["s"]; stale {
		t.Errorf("record still carries wrong-kind ix value: %v", rec.IX)
	}
}
