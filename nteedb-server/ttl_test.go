package main

import (
	"fmt"
	"testing"
	"time"
)

// pollUntil retries fn every few ms until it returns true or the deadline
// passes. The core's clock is real here, so server TTL tests use tiny real
// TTLs plus bounded polling.
func pollUntil(t *testing.T, d time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestPutexExpires(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	value := `{"a":1}`
	tc.raw(fmt.Sprintf("putex k 40 %d\r\n%s\r\n", len(value), value))
	if m := tc.readResp(); m["ok"] != true {
		t.Fatalf("putex failed: %v", m)
	}
	if r := tc.mustOK("has k"); r != true {
		t.Fatal("key missing right after putex")
	}
	if !pollUntil(t, 2*time.Second, func() bool { return tc.mustOK("has k") == false }) {
		t.Fatal("putex key never expired")
	}
	m := tc.cmd("get k")
	if m["ok"] != true || m["found"] != false {
		t.Fatalf("get after expiry = %v, want found:false", m)
	}
}

func TestPutClearsTTLOverWire(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	value := `{"a":1}`
	tc.raw(fmt.Sprintf("putex k 30 %d\r\n%s\r\n", len(value), value))
	if m := tc.readResp(); m["ok"] != true {
		t.Fatalf("putex failed: %v", m)
	}
	tc.mustOK(`put k {"b":2}`) // plain put clears the pending expiry
	time.Sleep(60 * time.Millisecond)
	if r := tc.mustOK("has k"); r != true {
		t.Fatal("plain put did not clear the TTL")
	}
}

func TestPutxWithTTL(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	ix, value := `{"traceId":"T1"}`, `{"kind":"x"}`
	tc.raw(fmt.Sprintf("putx k %d %d 40\r\n%s\r\n%s\r\n", len(ix), len(value), ix, value))
	if m := tc.readResp(); m["ok"] != true {
		t.Fatalf("putx+ttl failed: %v", m)
	}
	if got := keys(tc.mustOK("ix traceId T1")); len(got) != 1 {
		t.Fatalf("indexed key missing: %v", got)
	}
	if !pollUntil(t, 2*time.Second, func() bool { return tc.mustOK("has k") == false }) {
		t.Fatal("putx ttl key never expired")
	}
	// ixrec self-heals: the vanished key is dropped from record results.
	if rows, _ := tc.mustOK("ixrec traceId T1").([]any); len(rows) != 0 {
		t.Fatalf("ixrec returned expired rows: %v", rows)
	}
}

func TestFixedWindowOverWire(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	// First request arms a 80ms window; the count grows within it and the
	// mid-window ttl args do not re-arm it.
	if r := tc.mustOK("incr w 1 80"); r != float64(1) {
		t.Fatalf("first incr = %v", r)
	}
	if r := tc.mustOK("incr w 1 80"); r != float64(2) {
		t.Fatalf("second incr = %v", r)
	}
	// After expiry the window restarts at 1.
	if !pollUntil(t, 2*time.Second, func() bool {
		return tc.mustOK("incr w 1 80") == float64(1)
	}) {
		t.Fatal("window never restarted")
	}
}

func TestCounterTTLArgsValidation(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	tc.mustFail("putex k", "usage: putex <pk> <ttlms> <nbytes>")
	tc.mustFail("topup b 1 10 0", "ttlms must be a positive integer")
	tc.mustFail("take b 1 0 x", "ttlms must be a positive integer")
	// putex with a bad ttl still consumes its data block (stream stays in
	// sync) and then fails cleanly.
	tc.raw("putex k abc 3\r\nxyz\r\n")
	m := tc.readResp()
	if m["ok"] != false {
		t.Fatalf("putex with bad ttl succeeded: %v", m)
	}
	if r := tc.mustOK("ping"); r != "pong" {
		t.Fatal("connection desynced after putex ttl error")
	}
}
