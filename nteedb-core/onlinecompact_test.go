package nteedb

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestReadsProceedDuringCompactRewrite pins the online-compaction contract:
// while Compact's long rebuild phase runs (with the compaction gate raised),
// reads complete normally and writes pend until the compaction finishes.
func TestReadsProceedDuringCompactRewrite(t *testing.T) {
	db, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 10; i++ {
		if err := db.Put(fmt.Sprintf("key%02d", i), []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}

	// Pause the rebuild at its first record until released.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rewriteRecordHook = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	defer func() { rewriteRecordHook = nil }()

	compactDone := make(chan error, 1)
	go func() { compactDone <- db.Compact() }()
	<-entered // the rebuild is underway and the compaction gate is up

	// Reads must complete while the rebuild is in progress.
	readDone := make(chan error, 1)
	go func() {
		v, ok, err := db.Get("key03")
		if err == nil && (!ok || string(v) != `{"n":3}`) {
			err = fmt.Errorf("bad read during rebuild: %q %v", v, ok)
		}
		if err == nil {
			if keys, e := db.PrefixScan("key"); e != nil || len(keys) != 10 {
				err = fmt.Errorf("bad scan during rebuild: %d keys, %v", len(keys), e)
			}
		}
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read blocked during compact rebuild — reads must stay live")
	}

	// Writes must pend on the compaction gate until the compaction finishes.
	writeDone := make(chan error, 1)
	go func() { writeDone <- db.Put("key-during", []byte(`{"w":1}`)) }()
	select {
	case <-writeDone:
		t.Fatal("write completed during compact rebuild — compaction gate broken")
	case <-time.After(100 * time.Millisecond):
		// still pending, as it should be
	}

	close(release)
	if err := <-compactDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}

	// The gated write landed after the swap, on the compacted log.
	v, ok, err := db.Get("key-during")
	if err != nil || !ok || string(v) != `{"w":1}` {
		t.Fatalf("gated write lost after compact: %q %v %v", v, ok, err)
	}
	if keys, err := db.PrefixScan("key"); err != nil || len(keys) != 11 {
		t.Fatalf("post-compact scan: %d keys, %v", len(keys), err)
	}
}

func TestLiveBytes(t *testing.T) {
	db, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 4; i++ {
		if err := db.Put(fmt.Sprintf("k%d", i), []byte(`{"v":"aaaaaaaa"}`)); err != nil {
			t.Fatal(err)
		}
	}
	// Fresh store: every log byte is live.
	if live, main := db.LiveBytes(), db.Stats().MainBytes; live != main {
		t.Fatalf("fresh store: live %d != main %d", live, main)
	}

	// An overwrite and a delete create dead space (old line + tombstone line).
	if err := db.Put("k0", []byte(`{"v":"bbbbbbbb"}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete("k1"); err != nil {
		t.Fatal(err)
	}
	live, main := db.LiveBytes(), db.Stats().MainBytes
	if live >= main {
		t.Fatalf("after overwrite+delete: live %d should be < main %d", live, main)
	}
	if ratio := 1 - float64(live)/float64(main); ratio < 0.3 {
		t.Fatalf("dead ratio unexpectedly low: %.2f (live %d main %d)", ratio, live, main)
	}

	// Compact reclaims exactly the dead bytes: the log shrinks to LiveBytes.
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if live, main := db.LiveBytes(), db.Stats().MainBytes; live != main {
		t.Fatalf("post-compact: live %d != main %d", live, main)
	}
}
