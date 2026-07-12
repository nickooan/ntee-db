package nteedb

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFormatParseCounter(t *testing.T) {
	cases := []struct {
		v    int64
		want string
	}{
		{0, "+0000000000000000000"},
		{5, "+0000000000000000005"},
		{-5, "-0000000000000000005"},
		{math.MaxInt64, "+9223372036854775807"},
		{math.MinInt64, "-9223372036854775808"},
	}
	for _, c := range cases {
		got := formatCounter(c.v)
		if string(got) != c.want {
			t.Errorf("formatCounter(%d) = %q, want %q", c.v, got, c.want)
		}
		if len(got) != counterWidth {
			t.Errorf("formatCounter(%d) width %d, want %d", c.v, len(got), counterWidth)
		}
		back, ok := parseCounter(got)
		if !ok || back != c.v {
			t.Errorf("parseCounter(%q) = %d,%v, want %d,true", got, back, ok, c.v)
		}
	}
	bad := []string{
		"",
		"5",
		"0000000000000000005",  // no sign
		"+000000000000000005",  // 18 digits
		"+00000000000000000005", // 20 digits
		"+000000000000000000x",
		"+9223372036854775808", // MaxInt64+1
		"-9223372036854775809", // MinInt64-1
		" 0000000000000000005",
	}
	for _, s := range bad {
		if v, ok := parseCounter([]byte(s)); ok {
			t.Errorf("parseCounter(%q) = %d,true, want ok=false", s, v)
		}
	}
}

func TestIncrInitAndIncrDecr(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if v, err := db.Incr("c", 1); err != nil || v != 1 {
		t.Fatalf("first incr = %d,%v, want 1,nil", v, err)
	}
	if v, err := db.Incr("c", 41); err != nil || v != 42 {
		t.Fatalf("incr 41 = %d,%v, want 42,nil", v, err)
	}
	if v, err := db.Incr("c", -2); err != nil || v != 40 {
		t.Fatalf("decr 2 = %d,%v, want 40,nil", v, err)
	}
	if v, err := db.Incr("c", 0); err != nil || v != 40 {
		t.Fatalf("incr 0 (read) = %d,%v, want 40,nil", v, err)
	}
	// Init with negative delta on a missing key.
	if v, err := db.Incr("neg", -7); err != nil || v != -7 {
		t.Fatalf("init-decr = %d,%v, want -7,nil", v, err)
	}
}

// readMainLines returns the non-empty lines of main.jsonl.
func readMainLines(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, mainFile))
	if err != nil {
		t.Fatalf("read main.jsonl: %v", err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestIncrSignFlipFixedWidthOnDisk(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	defer db.Close()

	if _, err := db.Incr("c", 3); err != nil {
		t.Fatal(err)
	}
	lines := readMainLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("lines after init = %d, want 1", len(lines))
	}
	baseline := lines[0]
	if !strings.Contains(baseline, `"s":"+0000000000000000003"`) || !strings.Contains(baseline, `"c":true`) {
		t.Fatalf("unexpected record line: %s", baseline)
	}

	// Cross zero in both directions; the line count and length must not move.
	for _, delta := range []int64{-10, +20, -20, math.MaxInt64/2 + 3} {
		if _, err := db.Incr("c", delta); err != nil {
			t.Fatalf("incr %d: %v", delta, err)
		}
		lines = readMainLines(t, dir)
		if len(lines) != 1 {
			t.Fatalf("in-place incr appended: %d lines", len(lines))
		}
		if len(lines[0]) != len(baseline) {
			t.Fatalf("record length changed: %d -> %d (%s)", len(baseline), len(lines[0]), lines[0])
		}
	}
	if v, err := db.Incr("c", 0); err != nil || v != -7+math.MaxInt64/2+3 {
		t.Fatalf("final value = %d,%v", v, err)
	}
}

func TestIncrOverflowUnderflow(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if _, err := db.Incr("c", math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Incr("c", 1); !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("overflow err = %v, want ErrCounterOverflow", err)
	}
	if v, err := db.Incr("c", 0); err != nil || v != math.MaxInt64 {
		t.Fatalf("value after failed overflow = %d,%v, want MaxInt64", v, err)
	}

	if _, err := db.Incr("d", math.MinInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Incr("d", -1); !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("underflow err = %v, want ErrCounterOverflow", err)
	}
	if v, err := db.Incr("d", 0); err != nil || v != math.MinInt64 {
		t.Fatalf("value after failed underflow = %d,%v, want MinInt64", v, err)
	}
}

func TestIncrNotCounter(t *testing.T) {
	db, err := Open(Options{Dir: t.TempDir(), BlobThreshold: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Every non-counter value shape must be rejected untouched.
	shapes := map[string][]byte{
		"str":   []byte("hello"),
		"doc":   []byte(`{"a":1}`),
		"bin":   {0xff, 0xfe, 0x00},
		"float": []byte("1.5"),
		"fake":  []byte("+0000000000000000005"), // looks like a counter, isn't one
		"blob":  bytes.Repeat([]byte("x"), 64),  // over threshold → blob record
	}
	for key, val := range shapes {
		if err := db.Put(key, val); err != nil {
			t.Fatalf("put %q: %v", key, err)
		}
		if _, err := db.Incr(key, 1); !errors.Is(err, ErrNotCounter) {
			t.Errorf("incr on %q = %v, want ErrNotCounter", key, err)
		}
		got, ok := mustGet(t, db, key)
		if !ok || got != string(val) {
			t.Errorf("value of %q changed after rejected incr: %q", key, got)
		}
	}
}

func TestIncrInPlaceNoLogGrowth(t *testing.T) {
	for _, sync := range []bool{false, true} {
		db, err := Open(Options{Dir: t.TempDir(), SyncEveryWrite: sync})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Incr("c", 1); err != nil {
			t.Fatal(err)
		}
		base := db.Stats().MainBytes
		for i := 0; i < 100; i++ {
			if _, err := db.Incr("c", 7); err != nil {
				t.Fatal(err)
			}
		}
		if got := db.Stats().MainBytes; got != base {
			t.Errorf("sync=%v: MainBytes grew %d -> %d; in-place path not taken", sync, base, got)
		}
		if v, err := db.Incr("c", 0); err != nil || v != 701 {
			t.Errorf("sync=%v: value = %d,%v, want 701", sync, v, err)
		}
		db.Close()
	}
}

func TestIncrPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	if _, err := db.Incr("c", 5); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := db.Incr("c", 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen via the hint fast path (Close wrote a hint).
	db = mustOpen(t, dir)
	if v, err := db.Incr("c", 0); err != nil || v != 105 {
		t.Fatalf("after hint reopen = %d,%v, want 105", v, err)
	}
	if _, err := db.Incr("c", 1); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopen via the full-scan path (hint removed).
	if err := os.Remove(filepath.Join(dir, hintFile)); err != nil {
		t.Fatal(err)
	}
	db = mustOpen(t, dir)
	defer db.Close()
	if v, err := db.Incr("c", 0); err != nil || v != 106 {
		t.Fatalf("after full-scan reopen = %d,%v, want 106", v, err)
	}
}

func TestCompactPreservesCounter(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if err := db.Put("junk", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("junk", []byte("new")); err != nil { // dead version for compact to drop
		t.Fatal(err)
	}
	if _, err := db.Incr("c", 9); err != nil {
		t.Fatal(err)
	}
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if v, err := db.Incr("c", 1); err != nil || v != 10 {
		t.Fatalf("after compact = %d,%v, want 10", v, err)
	}
	// In-place still works against post-compaction offsets.
	base := db.Stats().MainBytes
	if _, err := db.Incr("c", 1); err != nil {
		t.Fatal(err)
	}
	if got := db.Stats().MainBytes; got != base {
		t.Errorf("MainBytes grew after post-compact incr: %d -> %d", base, got)
	}
}

func TestIncrFallbackWhenExtractIndexed(t *testing.T) {
	db, err := Open(Options{Dir: t.TempDir(), Indexes: []IndexDef{{
		Name: "firstByte",
		Kind: KindString,
		Extract: func(key string, value []byte) (any, bool) {
			if len(value) == 0 {
				return nil, false
			}
			return string(value[:1]), true
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Incr("c", 1); err != nil {
		t.Fatal(err)
	}
	base := db.Stats().MainBytes
	if v, err := db.Incr("c", -2); err != nil || v != -1 {
		t.Fatalf("incr = %d,%v, want -1", v, err)
	}
	if got := db.Stats().MainBytes; got <= base {
		t.Fatalf("expected append fallback to grow the log: %d -> %d", base, got)
	}
	// The Extract index must track the sign byte through the fallback writes.
	keys, err := db.ByIndex("firstByte", "-")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "c" {
		t.Fatalf(`ByIndex("firstByte","-") = %v, want ["c"]`, keys)
	}
	if keys, _ := db.ByIndex("firstByte", "+"); len(keys) != 0 {
		t.Fatalf(`stale "+" index entry survived: %v`, keys)
	}
}

func TestIncrDeleteIncr(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if _, err := db.Incr("c", 42); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete("c"); err != nil {
		t.Fatal(err)
	}
	if v, err := db.Incr("c", 3); err != nil || v != 3 {
		t.Fatalf("incr after delete = %d,%v, want fresh 3", v, err)
	}
}

func TestPutDemotesCounter(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if _, err := db.Incr("c", 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("c", []byte("plain")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Incr("c", 1); !errors.Is(err, ErrNotCounter) {
		t.Fatalf("incr after put = %v, want ErrNotCounter", err)
	}
}

func TestIncrConcurrent(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	const goroutines, per = 8, 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := db.Incr("c", 1); err != nil {
					t.Errorf("incr: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if v, err := db.Incr("c", 0); err != nil || v != goroutines*per {
		t.Fatalf("final = %d,%v, want %d", v, err, goroutines*per)
	}
}
