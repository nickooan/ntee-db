package nteedb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withFakeClock installs a deterministic clock and returns an advance func.
// TTL decisions all flow through nowMillis, so tests never sleep.
func withFakeClock(t *testing.T) func(ms int64) {
	t.Helper()
	real := nowMillis
	t.Cleanup(func() { nowMillis = real })
	now := int64(1_000_000_000_000)
	nowMillis = func() int64 { return now }
	return func(ms int64) { now += ms }
}

// waitGone polls until the reaper has removed key's pk entry (durable
// cleanup is async) or the deadline passes.
func waitGone(t *testing.T, db *DB, key string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		db.mu.RLock()
		_, ok := db.pk.get(key)
		db.mu.RUnlock()
		if !ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("key %q still in the pk index after 2s", key)
}

func TestTTLExpiryAcrossReadPaths(t *testing.T) {
	advance := withFakeClock(t)
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if err := db.Put("k", []byte(`{"a":1}`), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("stay", []byte(`{"b":2}`)); err != nil {
		t.Fatal(err)
	}
	// Live before expiry.
	if !db.Has("k") {
		t.Fatal("Has(k) = false before expiry")
	}
	if v, ok := mustGet(t, db, "k"); !ok || v != `{"a":1}` {
		t.Fatalf("Get before expiry = %q,%v", v, ok)
	}

	advance(5001)

	if db.Has("k") {
		t.Error("Has(k) = true after expiry")
	}
	if _, ok := mustGet(t, db, "k"); ok {
		t.Error("Get(k) found after expiry")
	}
	values, found, err := db.GetMany([]string{"k", "stay", "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if found[0] || !found[1] || found[2] {
		t.Errorf("GetMany found = %v, want [false true false]", found)
	}
	if string(values[1]) != `{"b":2}` {
		t.Errorf("GetMany live value = %q", values[1])
	}
	keys, err := db.PrefixScan("")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "stay" {
		t.Errorf("PrefixScan = %v, want [stay]", keys)
	}
	// The lazy hits queued the key; the reaper deletes it durably.
	waitGone(t, db, "k")
}

func TestPutClearsTTL(t *testing.T) {
	advance := withFakeClock(t)
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if err := db.Put("k", []byte("v1"), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("k", []byte("v2")); err != nil { // no ttl → clears it
		t.Fatal(err)
	}
	advance(60_000)
	if v, ok := mustGet(t, db, "k"); !ok || v != "v2" {
		t.Fatalf("value after TTL-clearing Put = %q,%v, want v2,true", v, ok)
	}
	// And a re-put with a ttl replaces it.
	if err := db.Put("k", []byte("v3"), time.Second); err != nil {
		t.Fatal(err)
	}
	advance(1001)
	if _, ok := mustGet(t, db, "k"); ok {
		t.Fatal("re-armed TTL did not expire")
	}
}

func TestInvalidTTL(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if err := db.Put("k", []byte("v"), -time.Second); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("negative ttl = %v, want ErrInvalidTTL", err)
	}
	if err := db.Put("k", []byte("v"), time.Second, time.Minute); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("two ttls = %v, want ErrInvalidTTL", err)
	}
	if _, err := db.Incr("c", 1, 0); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("zero ttl on incr = %v, want ErrInvalidTTL", err)
	}
	if err := db.PutBatch([]PutItem{{Key: "b", Value: []byte("v"), TTL: -1}}); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("negative batch ttl = %v, want ErrInvalidTTL", err)
	}
}

func TestPutBatchTTL(t *testing.T) {
	advance := withFakeClock(t)
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	err := db.PutBatch([]PutItem{
		{Key: "eph", Value: []byte("x"), TTL: time.Second},
		{Key: "perm", Value: []byte("y")},
	})
	if err != nil {
		t.Fatal(err)
	}
	advance(1001)
	if db.Has("eph") {
		t.Error("batch TTL item survived expiry")
	}
	if !db.Has("perm") {
		t.Error("batch item without TTL expired")
	}
}

func TestCounterWindowRestart(t *testing.T) {
	advance := withFakeClock(t)
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	// First request arms a 60s window (create-only ttl).
	if v, err := db.Incr("w", 1, time.Minute); err != nil || v != 1 {
		t.Fatalf("first incr = %d,%v", v, err)
	}
	// Subsequent requests count within it; the ttl arg is ignored for a live
	// key (window must not slide).
	for i := 0; i < 4; i++ {
		if _, err := db.Incr("w", 1, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := db.Incr("w", 0); v != 5 {
		t.Fatalf("window count = %d, want 5", v)
	}
	advance(30_000)
	if v, _ := db.Incr("w", 1, time.Minute); v != 6 {
		t.Fatalf("mid-window incr = %d, want 6 (ttl must not re-arm)", v)
	}
	// Past the ORIGINAL deadline: the window restarts at 1 with a new ttl.
	advance(30_001)
	if v, err := db.Incr("w", 1, time.Minute); err != nil || v != 1 {
		t.Fatalf("post-expiry incr = %d,%v, want fresh 1", v, err)
	}
	// And the new window expires on its own schedule.
	advance(60_001)
	if v, _ := db.Incr("w", 0, time.Minute); v != 0 {
		t.Fatalf("restarted window did not expire: %d", v)
	}
}

func TestCounterTTLSurvivesPatchAndReopen(t *testing.T) {
	advance := withFakeClock(t)
	dir := t.TempDir()
	db := mustOpen(t, dir)

	if _, err := db.Incr("c", 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	// In-place patches must not disturb the on-disk exp field.
	base := db.Stats().MainBytes
	for i := 0; i < 50; i++ {
		if _, err := db.Incr("c", 1); err != nil {
			t.Fatal(err)
		}
	}
	if got := db.Stats().MainBytes; got != base {
		t.Fatalf("MainBytes grew %d -> %d; TTL'd counter lost the in-place path", base, got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen via hint: exp must be restored.
	db = mustOpen(t, dir)
	advance(3_600_001)
	if v, err := db.Incr("c", 0, time.Hour); err != nil || v != 0 {
		t.Fatalf("counter after reopen+expiry = %d,%v, want fresh 0", v, err)
	}
	db.Close()

	// Reopen via full scan (hint removed): same story for a fresh TTL.
	db = mustOpen(t, dir)
	if _, err := db.Incr("c2", 7, time.Hour); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.Remove(filepath.Join(dir, hintFile)); err != nil {
		t.Fatal(err)
	}
	db = mustOpen(t, dir)
	defer db.Close()
	advance(3_600_001)
	if db.Has("c2") {
		t.Fatal("c2 survived expiry after full-scan reopen")
	}
}

func TestTopupTakeCreateTTL(t *testing.T) {
	advance := withFakeClock(t)
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	// Topup creates the bucket with a ttl; refusals never create.
	if over, err := db.Topup("b", 5, 10, time.Second); err != nil || over != 0 {
		t.Fatal(over, err)
	}
	if ok, err := db.Take("b", 3, 0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	advance(1001)
	if ok, _ := db.Take("b", 1, 0); ok {
		t.Fatal("take succeeded on an expired bucket")
	}
	// The failed take on the expired counter recreated nothing (take of 1
	// from 0 refuses), and a fresh topup re-arms.
	if over, err := db.Topup("b", 2, 10, time.Second); err != nil || over != 0 {
		t.Fatal(over, err)
	}
	if v, _ := db.Incr("b", 0); v != 2 {
		t.Fatalf("recreated bucket = %d, want 2", v)
	}
}

func TestReaperDurability(t *testing.T) {
	advance := withFakeClock(t)
	dir := t.TempDir()
	db := mustOpen(t, dir)

	if err := db.Put("k", []byte("v"), time.Second); err != nil {
		t.Fatal(err)
	}
	advance(1001)
	if db.Has("k") { // lazy miss + enqueue
		t.Fatal("expired key visible")
	}
	waitGone(t, db, "k")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Remove the hint to force a full log replay: the reaper's tombstone
	// must be in the log, keeping the key dead.
	if err := os.Remove(filepath.Join(dir, hintFile)); err != nil {
		t.Fatal(err)
	}
	db = mustOpen(t, dir)
	defer db.Close()
	db.mu.RLock()
	_, ok := db.pk.get("k")
	db.mu.RUnlock()
	if ok {
		t.Fatal("expired key resurrected after replay — tombstone missing")
	}
}

func TestReaperABA(t *testing.T) {
	advance := withFakeClock(t)
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	if err := db.Put("k", []byte("old"), time.Second); err != nil {
		t.Fatal(err)
	}
	db.mu.RLock()
	stale, _ := db.pk.get("k")
	db.mu.RUnlock()
	advance(1001)

	// The key is rewritten fresh AFTER the (stale) expiry observation.
	if err := db.Put("k", []byte("new")); err != nil {
		t.Fatal(err)
	}
	// Deliver the stale observation directly: it must not delete new data.
	db.reapBatch([]reapReq{{key: "k", off: stale.off}})
	if v, ok := mustGet(t, db, "k"); !ok || v != "new" {
		t.Fatalf("fresh value lost to a stale reap: %q,%v", v, ok)
	}
	// And a matching-but-live entry is also left alone.
	db.mu.RLock()
	fresh, _ := db.pk.get("k")
	db.mu.RUnlock()
	db.reapBatch([]reapReq{{key: "k", off: fresh.off}})
	if _, ok := mustGet(t, db, "k"); !ok {
		t.Fatal("live entry reaped")
	}
}

func TestExpiredIndexedKeyTransientlyVisibleByIndex(t *testing.T) {
	advance := withFakeClock(t)
	db, err := Open(Options{Dir: t.TempDir(), Indexes: []IndexDef{{Name: "tag", Kind: KindString}}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.PutIndexed("k", []byte(`{"a":1}`), IndexValues{"tag": "T"}, time.Second); err != nil {
		t.Fatal(err)
	}
	advance(1001)
	// Documented caveat: the secondary index is TTL-unaware, so the key is
	// still listed until cleanup — but any record fetch reports it missing.
	if keys, _ := db.ByIndex("tag", "T"); len(keys) != 1 {
		t.Fatalf("ByIndex before cleanup = %v (expected transient visibility)", keys)
	}
	if db.Has("k") {
		t.Fatal("Has(k) true after expiry")
	}
	waitGone(t, db, "k") // reaper retracts the secondary entry
	if keys, _ := db.ByIndex("tag", "T"); len(keys) != 0 {
		t.Fatalf("ByIndex after reap = %v, want empty", keys)
	}
}

func TestCompactDropsExpired(t *testing.T) {
	advance := withFakeClock(t)
	db, err := Open(Options{Dir: t.TempDir(), Indexes: []IndexDef{{Name: "tag", Kind: KindString}}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.PutIndexed("dead", []byte(`{"a":1}`), IndexValues{"tag": "T"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("live", []byte(`{"b":2}`)); err != nil {
		t.Fatal(err)
	}
	advance(1001)
	// No read ever touches "dead": Compact alone must drop it.
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	db.mu.RLock()
	_, ok := db.pk.get("dead")
	db.mu.RUnlock()
	if ok {
		t.Fatal("compact kept an expired key")
	}
	if keys, _ := db.ByIndex("tag", "T"); len(keys) != 0 {
		t.Fatalf("secondary entries survived compaction: %v", keys)
	}
	if !db.Has("live") {
		t.Fatal("compact lost a live key")
	}
	// The rewritten log must not contain the expired key at all.
	raw, err := os.ReadFile(filepath.Join(db.opts.Dir, mainFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"dead"`)) {
		t.Fatal("expired record still present in the compacted log")
	}
}

func TestTTLReopenPreservedViaHintAndScan(t *testing.T) {
	advance := withFakeClock(t)
	dir := t.TempDir()
	db := mustOpen(t, dir)
	if err := db.Put("k", []byte("v"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil { // writes a v2 hint
		t.Fatal(err)
	}

	// Hint path.
	db = mustOpen(t, dir)
	if !db.Has("k") {
		t.Fatal("key missing after hint reopen")
	}
	advance(3_600_001)
	if db.Has("k") {
		t.Fatal("TTL lost across hint reopen")
	}
	db.Close()
}

func TestOldHintVersionRejected(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir)
	if err := db.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Rewrite the hint's meta line as v1: the loader must reject it and the
	// boot must fall back to a full scan (data still complete).
	hintPath := filepath.Join(dir, hintFile)
	raw, err := os.ReadFile(hintPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := loadIndexHint(hintPath); !ok {
		t.Fatal("fresh hint unexpectedly rejected")
	}
	lines := raw
	first := []byte(`{"v":1,"covers":0}` + "\n")
	if err := os.WriteFile(hintPath, append(first, lines...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := loadIndexHint(hintPath); ok {
		t.Fatal("v1 hint accepted by a v2 loader")
	}
	db = mustOpen(t, dir)
	defer db.Close()
	if !db.Has("k") {
		t.Fatal("full-scan fallback lost data")
	}
}

func TestCloseStopsReaperUnderLoad(t *testing.T) {
	advance := withFakeClock(t)
	db := mustOpen(t, t.TempDir())

	for i := 0; i < 200; i++ {
		key := "k" + formatInt(i)
		if err := db.Put(key, []byte("v"), time.Second); err != nil {
			t.Fatal(err)
		}
	}
	advance(1001)
	// Flood the reaper queue via reads, then close immediately.
	for i := 0; i < 200; i++ {
		db.Has("k" + formatInt(i))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
}

func formatInt(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestExtremeExpiryValues(t *testing.T) {
	advance := withFakeClock(t)
	db := mustOpen(t, t.TempDir())
	defer db.Close()

	// A very long ttl never reads as expired.
	if err := db.Put("far", []byte("v"), 100*365*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	advance(24 * 3600 * 1000)
	if !db.Has("far") {
		t.Fatal("century TTL expired after a day")
	}
}
