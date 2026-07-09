package nteedb

import (
	"fmt"
	"sync"
	"testing"
)

// BenchmarkGetContention measures parallel Get latency with and without a
// Compact loop hammering the exclusive write lock. Get holds db.mu.RLock across
// its pread, so a Compact (which takes the exclusive Lock for its whole
// duration) stalls readers at RLock acquisition until it finishes.
//
// This documents a KNOWN limitation, not a live tuning knob. The "unlocked
// reads" idea (snapshot rf + read outside db.mu) was tried and reverted: it
// does not help, because readers block acquiring the RLock while Compact holds
// the exclusive Lock — the pread never being the thing under the lock is
// irrelevant. The real fix is online compaction (build the rewrite off the
// exclusive lock via a COW index clone, take the lock only to replay the tail
// and swap); see the note on Compact. r1quest compacts occasionally, so the
// stall this measures does not arise in practice.
func BenchmarkGetContention(b *testing.B) {
	const seed = 20_000
	keys := make([]string, seed)
	value := []byte("a modest api-call record payload, ~64 bytes give or take xx")
	setup := func(b *testing.B) *DB {
		db, err := Open(Options{Dir: b.TempDir()})
		if err != nil {
			b.Fatal(err)
		}
		for i := range keys {
			keys[i] = fmt.Sprintf("k%09d", i)
			if err := db.Put(keys[i], value); err != nil {
				b.Fatal(err)
			}
		}
		return db
	}

	b.Run("readers-only", func(b *testing.B) {
		db := setup(b)
		defer db.Close()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for i := 0; pb.Next(); i++ {
				if _, _, err := db.Get(keys[i%len(keys)]); err != nil {
					b.Fatal(err)
				}
			}
		})
	})

	b.Run("readers+compaction", func(b *testing.B) {
		db := setup(b)
		defer db.Close()
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = db.Compact() // holds the exclusive Lock for its duration
				}
			}
		}()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for i := 0; pb.Next(); i++ {
				if _, _, err := db.Get(keys[i%len(keys)]); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.StopTimer()
		close(stop)
		wg.Wait()
	})
}

func benchSeed(b *testing.B, n int) (dir string, db *DB) {
	b.Helper()
	dir = b.TempDir()
	db, err := Open(Options{Dir: dir})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := db.Put(fmt.Sprintf("key%07d", i), []byte("value")); err != nil {
			b.Fatal(err)
		}
	}
	return dir, db
}

func BenchmarkPut(b *testing.B) {
	db, err := Open(Options{Dir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = db.Put(fmt.Sprintf("key%07d", i), []byte("value"))
	}
}

// BenchmarkPutHintEveryN measures the write path with periodic hint rewrites
// enabled (the app uses HintEveryN=5), including the every-Nth-write hint cost.
func BenchmarkPutHintEveryN(b *testing.B) {
	db, err := Open(Options{Dir: b.TempDir(), HintEveryN: 5})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = db.Put(fmt.Sprintf("key%07d", i), []byte("value"))
	}
}

// BenchmarkPutHintEveryNSeeded runs the HintEveryN write path on a store already
// holding `seed` records. With the COW clone the every-Nth-write snapshot is
// O(1), so per-op cost must stay flat as `seed` grows (the old full-slice+map
// copy scaled O(seed/N) per write). Compare the 10k and 100k rows.
func BenchmarkPutHintEveryNSeeded(b *testing.B) {
	for _, seed := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("seed=%d", seed), func(b *testing.B) {
			db, err := Open(Options{Dir: b.TempDir(), HintEveryN: 5})
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			for i := 0; i < seed; i++ {
				if err := db.Put(fmt.Sprintf("key%09d", i), []byte("value")); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = db.Put(fmt.Sprintf("key%09d", seed+i), []byte("value"))
			}
		})
	}
}

// BenchmarkPutIndexedHotValue guards the sorted-slice insert cliff: 50k seeded
// entries share index value "zzz", and every benchmarked write carries value
// "aaa" — sorting BEFORE the whole zzz block, so each insert memmoves all 50k
// entries (O(n) per write; O(N²) to build such an index). If this row degrades
// sharply vs BenchmarkPut, the btree upgrade contemplated in secindex.go is due.
func BenchmarkPutIndexedHotValue(b *testing.B) {
	db, err := Open(Options{Dir: b.TempDir(), Indexes: []IndexDef{{Name: "v", Kind: KindString}}})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	const seed = 50_000
	items := make([]PutItem, seed)
	for i := range items {
		items[i] = PutItem{
			Key:   fmt.Sprintf("z%08d", i),
			Value: []byte("value"),
			IX:    IndexValues{"v": "zzz"},
		}
	}
	if err := db.PutBatch(items); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = db.PutIndexed(fmt.Sprintf("a%08d", i), []byte("value"), IndexValues{"v": "aaa"})
	}
}

func BenchmarkGet(b *testing.B) {
	_, db := benchSeed(b, 10000)
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = db.Get(fmt.Sprintf("key%07d", i%10000))
	}
}

func BenchmarkPrefixScan(b *testing.B) {
	_, db := benchSeed(b, 10000)
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = db.PrefixScan("key000001") // ~10 matching keys
	}
}

// BenchmarkBoot measures reopen time (hint load + tail replay) at scale.
func BenchmarkBoot(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			dir, db := benchSeed(b, n)
			db.Close() // writes a full hint
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				d, err := Open(Options{Dir: dir})
				if err != nil {
					b.Fatal(err)
				}
				d.Close()
			}
		})
	}
}
