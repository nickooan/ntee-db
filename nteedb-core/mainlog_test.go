package nteedb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendAndScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.jsonl")
	l, err := openMainLog(path, false)
	if err != nil {
		t.Fatal(err)
	}

	recs := []record{
		{Key: "a", Value: []byte("1")},
		{Key: "b", Value: []byte("two")},
		{Key: "a", Deleted: true},
	}
	type loc struct {
		off int64
		n   int32
	}
	var locs []loc
	for _, r := range recs {
		off, n, err := l.append(r)
		if err != nil {
			t.Fatal(err)
		}
		locs = append(locs, loc{off, n})
	}
	if err := l.close(); err != nil {
		t.Fatal(err)
	}

	// Offsets must be sequential and contiguous.
	var want int64
	for i, lc := range locs {
		if lc.off != want {
			t.Errorf("rec %d: off = %d, want %d", i, lc.off, want)
		}
		want += int64(lc.n)
	}

	// Scan should yield the same records at the same offsets.
	var got []record
	end, err := scanMainLog(path, 0, func(r record, off int64, n int32) error {
		if off != locs[len(got)].off || n != locs[len(got)].n {
			t.Errorf("rec %d: scan loc {%d,%d}, want {%d,%d}", len(got), off, n, locs[len(got)].off, locs[len(got)].n)
		}
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if end != want {
		t.Errorf("goodEnd = %d, want %d", end, want)
	}
	if len(got) != len(recs) {
		t.Fatalf("scanned %d records, want %d", len(got), len(recs))
	}
	if got[0].Key != "a" || string(got[1].Value) != "two" || !got[2].Deleted {
		t.Errorf("unexpected records: %+v", got)
	}
}

func TestScanFromOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.jsonl")
	l, _ := openMainLog(path, false)
	_, n0, _ := l.append(record{Key: "a", Value: []byte("1")})
	l.append(record{Key: "b", Value: []byte("2")})
	l.close()

	// Replaying only the tail after the first record should yield just "b".
	var got []record
	_, err := scanMainLog(path, int64(n0), func(r record, off int64, n int32) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "b" {
		t.Fatalf("tail scan = %+v, want [b]", got)
	}
}

func TestScanTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.jsonl")
	l, _ := openMainLog(path, false)
	_, n0, _ := l.append(record{Key: "a", Value: []byte("1")})
	l.close()
	goodSize := int64(n0)

	// Simulate a crash mid-append: a partial line with no trailing newline.
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	f.WriteString(`{"k":"b","v":`)
	f.Close()

	var got []record
	end, err := scanMainLog(path, 0, func(r record, off int64, n int32) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "a" {
		t.Errorf("scan = %+v, want only [a]", got)
	}
	if end != goodSize {
		t.Errorf("goodEnd = %d, want %d (so caller truncates the torn tail)", end, goodSize)
	}
}

func TestScanMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	end, err := scanMainLog(path, 0, func(record, int64, int32) error {
		t.Fatal("fn must not be called for a missing file")
		return nil
	})
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if end != 0 {
		t.Errorf("goodEnd = %d, want 0", end)
	}
}
