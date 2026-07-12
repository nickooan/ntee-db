package nteedb

import (
	"bytes"
	"fmt"
	"testing"
)

func TestPutBatchOrderAndQueries(t *testing.T) {
	db := openIndexed(t, t.TempDir())
	defer db.Close()

	err := db.PutBatch([]PutItem{
		{Key: "call:1", Value: []byte(`{"v":"a"}`), IX: IndexValues{"traceId": "T1", "status": 200}},
		{Key: "call:2", Value: []byte(`{"v":"b"}`), IX: IndexValues{"traceId": "T1", "status": 404}},
		{Key: "call:3", Value: []byte(`{"v":"c"}`), IX: IndexValues{"traceId": "T2"}},
		{Key: "call:1", Value: []byte("a2")}, // same key later in the batch wins
	})
	if err != nil {
		t.Fatal(err)
	}

	if v, _, _ := db.Get("call:1"); string(v) != "a2" {
		t.Errorf("call:1 = %q, want a2 (last write in batch wins)", v)
	}
	// The overwrite carried no index values, so call:1's old entries retract.
	if got := mustBy(t, db, "traceId", "T1"); !eqStrs(got, []string{"call:2"}) {
		t.Errorf("traceId T1 = %v, want [call:2]", got)
	}
	if got := mustBy(t, db, "traceId", "T2"); !eqStrs(got, []string{"call:3"}) {
		t.Errorf("traceId T2 = %v", got)
	}
}

func TestPutBatchValidationWritesNothing(t *testing.T) {
	db := openIndexed(t, t.TempDir())
	defer db.Close()

	err := db.PutBatch([]PutItem{
		{Key: "ok:1", Value: []byte("v")},
		{Key: "bad:1", Value: []byte(`{}`), IX: IndexValues{"status": "not-a-number"}},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if db.Has("ok:1") || db.Has("bad:1") {
		t.Error("a failed-validation batch must write nothing")
	}
}

func TestPutBatchMaxPerValueAndDurability(t *testing.T) {
	dir := t.TempDir()
	db := openCapped(t, dir, 2) // traceId capped at 2 per value

	items := make([]PutItem, 5)
	for i := range items {
		items[i] = PutItem{
			Key:   fmt.Sprintf("call:%d", i+1),
			Value: []byte(`{"v":"v"}`),
			IX:    IndexValues{"traceId": "T"},
		}
	}
	if err := db.PutBatch(items); err != nil {
		t.Fatal(err)
	}
	// A batch overflowing one value keeps only the cap's newest records.
	if got := mustBy(t, db, "traceId", "T"); !eqStrs(got, []string{"call:4", "call:5"}) {
		t.Errorf("traceId T = %v, want [call:4 call:5]", got)
	}
	db.Close()

	// The batch and its evictions survive a reopen.
	db2 := openCapped(t, dir, 2)
	defer db2.Close()
	if got := mustBy(t, db2, "traceId", "T"); !eqStrs(got, []string{"call:4", "call:5"}) {
		t.Errorf("after reopen traceId T = %v", got)
	}
	if db2.Has("call:1") {
		t.Error("evicted batch item resurfaced after reopen")
	}
}

func TestPutBatchBlobAndEmpty(t *testing.T) {
	db, err := Open(Options{Dir: t.TempDir(), BlobThreshold: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.PutBatch(nil); err != nil {
		t.Fatalf("empty batch should be a no-op: %v", err)
	}

	big := bytes.Repeat([]byte("x"), 64) // above the 8-byte blob threshold
	if err := db.PutBatch([]PutItem{
		{Key: "small", Value: []byte("v")},
		{Key: "big", Value: big},
	}); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := db.Get("big"); !bytes.Equal(v, big) {
		t.Error("blob batch item round-trip failed")
	}
	if v, _, _ := db.Get("small"); string(v) != "v" {
		t.Error("inline batch item round-trip failed")
	}
}

// BenchmarkPutBatch measures the amortized per-record cost of batched writes
// (batches of 1000), for comparison against BenchmarkPut.
func BenchmarkPutBatch(b *testing.B) {
	db, err := Open(Options{Dir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	const batchSize = 1000
	items := make([]PutItem, batchSize)
	b.ResetTimer()
	for n := 0; n < b.N; n += batchSize {
		for i := range items {
			items[i] = PutItem{Key: fmt.Sprintf("key%09d", n+i), Value: []byte("value")}
		}
		if err := db.PutBatch(items); err != nil {
			b.Fatal(err)
		}
	}
}
