package nteedb

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRecordRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		rec  record
	}{
		{"inline", record{Key: "GetOrders", Value: []byte("hello world")}},
		{"empty value", record{Key: "EmptyVal"}},
		{"blob ref", record{Key: "BigUpload", Blob: &blobRef{Off: 4096, Size: 1 << 20}}},
		{"tombstone", record{Key: "Gone", Deleted: true}},
		{"binary value", record{Key: "Bin", Value: []byte{0x00, 0x01, 0xff, 0x0a, 0x7f}}},
		{"newline in key", record{Key: "weird\nkey", Value: []byte("v")}},
		{"with index values", record{Key: "Req", Value: []byte("v"), IX: map[string]any{"traceId": "abc"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, err := marshalRecord(tc.rec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if bytes.ContainsRune(line, '\n') {
				t.Fatalf("marshaled line must not contain a literal newline: %q", line)
			}
			got, err := unmarshalRecord(line)
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Key != tc.rec.Key {
				t.Errorf("key: got %q want %q", got.Key, tc.rec.Key)
			}
			if got.Deleted != tc.rec.Deleted {
				t.Errorf("deleted: got %v want %v", got.Deleted, tc.rec.Deleted)
			}
			if !bytes.Equal(got.Value, tc.rec.Value) {
				t.Errorf("value: got %q want %q", got.Value, tc.rec.Value)
			}
			if (got.Blob == nil) != (tc.rec.Blob == nil) {
				t.Fatalf("blob presence mismatch: got %v want %v", got.Blob, tc.rec.Blob)
			}
			if got.Blob != nil && *got.Blob != *tc.rec.Blob {
				t.Errorf("blob: got %+v want %+v", *got.Blob, *tc.rec.Blob)
			}
			if len(got.IX) != len(tc.rec.IX) {
				t.Errorf("ix: got %v want %v", got.IX, tc.rec.IX)
			}
			for k, v := range tc.rec.IX {
				if fmt.Sprint(got.IX[k]) != fmt.Sprint(v) {
					t.Errorf("ix[%q]: got %v want %v", k, got.IX[k], v)
				}
			}
		})
	}
}

// TestRecordValueEdgeCases pushes the s/v auto-detect through the nasty
// inputs: every case must round-trip byte-exactly whichever field carries it.
func TestRecordValueEdgeCases(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"endpoint":"/api/users","status":200}`), // JSON text
		[]byte("line1\nline2\t\"quoted\""),               // escapes
		{0x00},                                           // NUL alone: valid UTF-8, escapes as \u0000
		[]byte("\x1b[31mred\x1b[0m"),                     // ANSI escape sequence
		[]byte("emoji 🎉 and 中文"),                         // multibyte
		{0xff, 0xfe, 'a'},                                // invalid UTF-8 → must stay base64
		{0xc3, 0x28},                                     // truncated multibyte → invalid → base64
	}
	for i, value := range cases {
		line, err := marshalRecord(record{Key: "k", Value: value})
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		got, err := unmarshalRecord(line)
		if err != nil {
			t.Fatalf("case %d unmarshal: %v", i, err)
		}
		if !bytes.Equal(got.Value, value) {
			t.Errorf("case %d: round-trip mismatch: got %x want %x", i, got.Value, value)
		}
	}
}

// TestRecordFormatChoice asserts which on-disk field carries the value: the
// readable "s" string for valid UTF-8, base64 "v" only for binary.
func TestRecordFormatChoice(t *testing.T) {
	text, _ := marshalRecord(record{Key: "k", Value: []byte(`{"a":1}`)})
	if !bytes.Contains(text, []byte(`"s":`)) || bytes.Contains(text, []byte(`"v":`)) {
		t.Errorf("text value should marshal to the s field: %s", text)
	}
	if !bytes.Contains(text, []byte(`{\"a\":1}`)) {
		t.Errorf("text payload should be readable in the line: %s", text)
	}

	bin, _ := marshalRecord(record{Key: "k", Value: []byte{0xff, 0x00}})
	if bytes.Contains(bin, []byte(`"s":`)) || !bytes.Contains(bin, []byte(`"v":`)) {
		t.Errorf("binary value should marshal to the v field: %s", bin)
	}
}

// TestOldFormatValueReadAndMigrated proves logs written before the "s" field
// still read correctly, and that compaction migrates them to the readable form.
func TestOldFormatValueReadAndMigrated(t *testing.T) {
	dir := t.TempDir()
	// Hand-write an old-format line: a TEXT value stored base64 in "v", exactly
	// as pre-"s" binaries wrote it.
	old := `{"k":"legacy","v":"` + base64of("hello old world") + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, mainFile), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	db := mustOpen(t, dir)
	defer db.Close()
	if v, ok := mustGet(t, db, "legacy"); !ok || v != "hello old world" {
		t.Fatalf("legacy = %q %v, want old-format value decoded", v, ok)
	}

	// Compaction's read-transform-write pass rewrites it in the new form.
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, mainFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"s":"hello old world"`)) {
		t.Errorf("compacted log should carry the readable form: %s", raw)
	}
	if v, ok := mustGet(t, db, "legacy"); !ok || v != "hello old world" {
		t.Errorf("after compaction legacy = %q %v", v, ok)
	}
}

func base64of(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func TestTombstoneDetection(t *testing.T) {
	if !(record{Key: "x", Deleted: true}).isTombstone() {
		t.Error("expected tombstone")
	}
	if (record{Key: "x", Value: []byte("v")}).isTombstone() {
		t.Error("live record reported as tombstone")
	}
}
