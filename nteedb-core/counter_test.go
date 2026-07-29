package nteedb

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
		"0000000000000000005",   // no sign
		"+000000000000000005",   // 18 digits
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

// Counters never participate in secondary indexes: even with a permissive
// Extract index declared, increments patch in place (no log growth) and the
// counter never appears in the index — while ordinary object records on the
// same store still get indexed.
func TestIncrIgnoresExtractIndexes(t *testing.T) {
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
	for _, delta := range []int64{-2, 100, -200} { // sign flips included
		if _, err := db.Incr("c", delta); err != nil {
			t.Fatal(err)
		}
	}
	if got := db.Stats().MainBytes; got != base {
		t.Fatalf("incr appended despite Extract index: MainBytes %d -> %d", base, got)
	}
	for _, val := range []string{"+", "-"} {
		if keys, _ := db.ByIndex("firstByte", val); len(keys) != 0 {
			t.Fatalf("counter leaked into index at %q: %v", val, keys)
		}
	}
	// Ordinary object records on the same store still index normally.
	if err := db.Put("doc", []byte(`{"kind":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if keys, _ := db.ByIndex("firstByte", "{"); len(keys) != 1 || keys[0] != "doc" {
		t.Fatalf("object record missing from index: %v", keys)
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

func TestTopupTakeBasics(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if _, err := db.Incr("c", 98); err != nil {
		t.Fatal(err)
	}
	// 98 + 5 vs max 100: fills to 100, 3 units overflow.
	if over, err := db.Topup("c", 5, 100); err != nil || over != 3 {
		t.Fatalf("topup 5 at 98 = %d,%v, want 3,nil", over, err)
	}
	if v, _ := db.Incr("c", 0); v != 100 {
		t.Fatalf("value after partial topup = %d, want 100", v)
	}
	// Already at max: nothing added, the whole amount overflows.
	if over, err := db.Topup("c", 5, 100); err != nil || over != 5 {
		t.Fatalf("topup at max = %d,%v, want 5,nil", over, err)
	}
	if v, _ := db.Incr("c", 0); v != 100 {
		t.Fatalf("value after refused topup = %d, want 100", v)
	}
	// 90 + 10 = 100 == max: exact fit, overflow 0.
	if _, err := db.Incr("c", -10); err != nil {
		t.Fatal(err)
	}
	if over, err := db.Topup("c", 10, 100); err != nil || over != 0 {
		t.Fatalf("topup at 90 = %d,%v, want 0,nil", over, err)
	}
	if v, _ := db.Incr("c", 0); v != 100 {
		t.Fatalf("value after topup = %d, want 100", v)
	}
	// Already above max: left unchanged (Topup only ever adds).
	if _, err := db.Incr("c", 20); err != nil {
		t.Fatal(err)
	}
	if over, err := db.Topup("c", 5, 100); err != nil || over != 5 {
		t.Fatalf("topup above max = %d,%v, want 5,nil", over, err)
	}
	if v, _ := db.Incr("c", 0); v != 120 {
		t.Fatalf("value above max changed: %d, want 120", v)
	}
	if _, err := db.Incr("c", -20); err != nil {
		t.Fatal(err)
	}
	// 100 - 100 = 0 == left: boundary applies.
	if ok, err := db.Take("c", 100, 0); err != nil || !ok {
		t.Fatalf("take 100 at 100 = %v,%v, want true,nil", ok, err)
	}
	if v, _ := db.Incr("c", 0); v != 0 {
		t.Fatalf("value after take = %d, want 0", v)
	}
	// 9 - 10 = -1 < 0: refused.
	if _, err := db.Incr("c", 9); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.Take("c", 10, 0); err != nil || ok {
		t.Fatalf("take 10 at 9 = %v,%v, want false,nil", ok, err)
	}
	if v, _ := db.Incr("c", 0); v != 9 {
		t.Fatalf("value after refused take = %d, want 9", v)
	}
	// 11 - 10 = 1 >= 0: applies.
	if _, err := db.Incr("c", 2); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.Take("c", 10, 0); err != nil || !ok {
		t.Fatalf("take 10 at 11 = %v,%v, want true,nil", ok, err)
	}
	if v, _ := db.Incr("c", 0); v != 1 {
		t.Fatalf("value after take = %d, want 1", v)
	}
}

func TestTopupTakeMissingKey(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	defer db.Close()

	// Topup on a missing key counts from 0 and creates it.
	if over, err := db.Topup("new", 5, 10); err != nil || over != 0 {
		t.Fatalf("topup missing = %d,%v, want 0,nil", over, err)
	}
	if v, _ := db.Incr("new", 0); v != 5 {
		t.Fatalf("created value = %d, want 5", v)
	}
	// A missing key clamps against max too: created at max, rest overflows.
	if over, err := db.Topup("clamped", 11, 10); err != nil || over != 1 {
		t.Fatalf("clamped topup missing = %d,%v, want 1,nil", over, err)
	}
	if v, _ := db.Incr("clamped", 0); v != 10 {
		t.Fatalf("clamped value = %d, want 10", v)
	}
	lines := len(readMainLines(t, dir))

	// Ops that add or take nothing must not create keys or touch the log.
	if over, err := db.Topup("miss", 5, 0); err != nil || over != 5 {
		t.Fatalf("topup with max 0 missing = %d,%v, want 5,nil", over, err)
	}
	if over, err := db.Topup("miss2", 0, 10); err != nil || over != 0 {
		t.Fatalf("zero topup missing = %d,%v, want 0,nil", over, err)
	}
	if ok, err := db.Take("miss3", 1, 0); err != nil || ok {
		t.Fatalf("refused take missing = %v,%v, want false,nil", ok, err)
	}
	for _, key := range []string{"miss", "miss2", "miss3"} {
		if db.Has(key) {
			t.Errorf("no-op created key %q", key)
		}
	}
	if got := len(readMainLines(t, dir)); got != lines {
		t.Fatalf("no-op writes grew log: %d -> %d lines", lines, got)
	}

	// A zero take with left <= 0 applies and creates the counter at 0.
	if ok, err := db.Take("zero", 0, 0); err != nil || !ok {
		t.Fatalf("take 0,0 missing = %v,%v, want true,nil", ok, err)
	}
	if v, _ := db.Incr("zero", 0); v != 0 {
		t.Fatalf("created value = %d, want 0", v)
	}
}

func TestTopupTakeNegativeAmount(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if _, err := db.Incr("c", 5); err != nil {
		t.Fatal(err)
	}
	if over, err := db.Topup("c", -1, 100); !errors.Is(err, ErrNegativeAmount) || over != -1 {
		t.Fatalf("topup negative = %d,%v, want -1,ErrNegativeAmount", over, err)
	}
	if _, err := db.Take("c", -1, 0); !errors.Is(err, ErrNegativeAmount) {
		t.Fatalf("take negative = %v, want ErrNegativeAmount", err)
	}
	if v, _ := db.Incr("c", 0); v != 5 {
		t.Fatalf("value after rejected ops = %d, want 5", v)
	}
}

func TestTopupTakeNotCounter(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if err := db.Put("s", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if over, err := db.Topup("s", 1, 10); !errors.Is(err, ErrNotCounter) || over != -1 {
		t.Fatalf("topup on string = %d,%v, want -1,ErrNotCounter", over, err)
	}
	if _, err := db.Take("s", 1, 0); !errors.Is(err, ErrNotCounter) {
		t.Fatalf("take on string = %v, want ErrNotCounter", err)
	}
	if got, ok := mustGet(t, db, "s"); !ok || got != "hello" {
		t.Fatalf("value changed after rejected ops: %q", got)
	}
}

// Out-of-int64-range arithmetic never errors: topup clamps at max, take
// refuses (a difference past MinInt64 is below every left bound).
func TestTopupTakeExtremeRange(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if _, err := db.Incr("c", math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if over, err := db.Topup("c", 1, math.MaxInt64); err != nil || over != 1 {
		t.Fatalf("topup at MaxInt64 = %d,%v, want 1,nil", over, err)
	}
	if v, _ := db.Incr("c", 0); v != math.MaxInt64 {
		t.Fatalf("value = %d, want MaxInt64", v)
	}
	// cur+amount would overflow int64: still a clean partial fill to max.
	if _, err := db.Incr("e", math.MaxInt64-2); err != nil {
		t.Fatal(err)
	}
	if over, err := db.Topup("e", 5, math.MaxInt64); err != nil || over != 3 {
		t.Fatalf("overflowing topup = %d,%v, want 3,nil", over, err)
	}
	if v, _ := db.Incr("e", 0); v != math.MaxInt64 {
		t.Fatalf("value = %d, want MaxInt64", v)
	}

	if _, err := db.Incr("d", math.MinInt64); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.Take("d", 1, math.MinInt64); err != nil || ok {
		t.Fatalf("take at MinInt64 = %v,%v, want false,nil", ok, err)
	}
	if v, _ := db.Incr("d", 0); v != math.MinInt64 {
		t.Fatalf("value = %d, want MinInt64", v)
	}
}

func TestTopupInPlaceNoLogGrowth(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if _, err := db.Incr("c", 1); err != nil {
		t.Fatal(err)
	}
	base := db.Stats().MainBytes
	for i := 0; i < 100; i++ {
		if over, err := db.Topup("c", 1, 1000); err != nil || over != 0 {
			t.Fatalf("topup %d = %d,%v", i, over, err)
		}
		if ok, err := db.Take("c", 5000, 0); err != nil || ok {
			t.Fatalf("refused take %d = %v,%v", i, ok, err)
		}
	}
	if got := db.Stats().MainBytes; got != base {
		t.Errorf("MainBytes grew %d -> %d; in-place path not taken", base, got)
	}
	if v, err := db.Incr("c", 0); err != nil || v != 101 {
		t.Errorf("value = %d,%v, want 101", v, err)
	}
}

func TestTopupPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	if over, err := db.Topup("c", 40, 100); err != nil || over != 0 {
		t.Fatal(over, err)
	}
	if over, err := db.Topup("c", 60, 100); err != nil || over != 0 {
		t.Fatal(over, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = mustOpen(t, dir)
	defer db.Close()
	if v, err := db.Incr("c", 0); err != nil || v != 100 {
		t.Fatalf("after reopen = %d,%v, want 100", v, err)
	}
	if ok, err := db.Take("c", 100, 0); err != nil || !ok {
		t.Fatalf("take after reopen = %v,%v, want true,nil", ok, err)
	}
}

// TestTakeConcurrentDrain races takers against a fixed stock: exactly stock
// takes may succeed, and the counter must land on 0 with no interleaving lost.
func TestTakeConcurrentDrain(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	const stock = 100
	if _, err := db.Incr("c", stock); err != nil {
		t.Fatal(err)
	}
	const goroutines, per = 8, 200
	var succeeded atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				ok, err := db.Take("c", 1, 0)
				if err != nil {
					t.Errorf("take: %v", err)
					return
				}
				if ok {
					succeeded.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if got := succeeded.Load(); got != stock {
		t.Fatalf("successful takes = %d, want %d", got, stock)
	}
	if v, err := db.Incr("c", 0); err != nil || v != 0 {
		t.Fatalf("final = %d,%v, want 0", v, err)
	}
}

func TestTopupConcurrentFill(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	const capacity = 100
	const goroutines, per = 8, 200
	var succeeded atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				over, err := db.Topup("c", 1, capacity)
				if err != nil {
					t.Errorf("topup: %v", err)
					return
				}
				if over == 0 {
					succeeded.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if got := succeeded.Load(); got != capacity {
		t.Fatalf("successful topups = %d, want %d", got, capacity)
	}
	if v, err := db.Incr("c", 0); err != nil || v != capacity {
		t.Fatalf("final = %d,%v, want %d", v, err, capacity)
	}
}

// BenchmarkIncr measures the in-place counter patch (read + pwrite under the
// write lock), for comparison against BenchmarkPut's append path.
func BenchmarkIncr(b *testing.B) {
	db, err := Open(Options{Dir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Incr("c", 1); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Incr("c", 1); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTopup measures the applied path: the bound check plus the same
// in-place patch as Incr (max is never reached, so every op writes).
func BenchmarkTopup(b *testing.B) {
	db, err := Open(Options{Dir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Incr("c", 1); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if over, err := db.Topup("c", 1, math.MaxInt64); err != nil || over != 0 {
			b.Fatal(over, err)
		}
	}
}

// BenchmarkTopupRefused measures the refused path: read and compare only,
// nothing written (the counter sits at or above max on every op).
func BenchmarkTopupRefused(b *testing.B) {
	db, err := Open(Options{Dir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Incr("c", 1); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if over, err := db.Topup("c", 1, 0); err != nil || over != 1 {
			b.Fatal(over, err)
		}
	}
}

// BenchmarkTake measures the applied path (seeded far above the floor so
// b.N iterations can never drain it).
func BenchmarkTake(b *testing.B) {
	db, err := Open(Options{Dir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Incr("c", math.MaxInt64); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ok, err := db.Take("c", 1, 0); err != nil || !ok {
			b.Fatal(ok, err)
		}
	}
}

// BenchmarkTakeRefused measures the refused path: the counter stays at 0, so
// every take is rejected before any write.
func BenchmarkTakeRefused(b *testing.B) {
	db, err := Open(Options{Dir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Incr("c", 0); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ok, err := db.Take("c", 1, 0); err != nil || ok {
			b.Fatal(ok, err)
		}
	}
}

// BenchmarkIncrDurable is the same with per-write fsync, the durability mode's
// floor for every write op.
func BenchmarkIncrDurable(b *testing.B) {
	db, err := Open(Options{Dir: b.TempDir(), SyncEveryWrite: true})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Incr("c", 1); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Incr("c", 1); err != nil {
			b.Fatal(err)
		}
	}
}
