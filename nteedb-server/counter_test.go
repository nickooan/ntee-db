package main

import (
	"testing"
)

func TestIncrDecrCommands(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	// Default delta is 1; result is a JSON number (json.Unmarshal → float64).
	if r := tc.mustOK("incr c"); r != float64(1) {
		t.Errorf("incr c = %v, want 1", r)
	}
	if r := tc.mustOK("incr c 41"); r != float64(42) {
		t.Errorf("incr c 41 = %v, want 42", r)
	}
	if r := tc.mustOK("decr c"); r != float64(41) {
		t.Errorf("decr c = %v, want 41", r)
	}
	if r := tc.mustOK("decr c 50"); r != float64(-9) {
		t.Errorf("decr c 50 = %v, want -9", r)
	}
	// Negative delta on incr decrements too.
	if r := tc.mustOK("incr c -1"); r != float64(-10) {
		t.Errorf("incr c -1 = %v, want -10", r)
	}
	// A zero result must render as the number 0, not be dropped by omitempty.
	if r := tc.mustOK("incr z 0"); r != float64(0) {
		t.Errorf("incr z 0 = %v, want 0", r)
	}
}

func TestIncrDecrArgErrors(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	tc.mustFail("incr", "usage: incr <pk> [delta]")
	tc.mustFail("incr c 1 2", "usage: incr <pk> [delta]")
	tc.mustFail("incr c 1.5", "delta must be an integer")
	tc.mustFail("incr c abc", "delta must be an integer")
	tc.mustFail("decr c 1.5", "delta must be an integer")
	tc.mustFail("decr c -9223372036854775808", "delta out of range")

	// Type rule: incr on a non-counter value fails and leaves it untouched.
	tc.mustOK(`put s {"a":1}`)
	tc.mustFail("incr s", "non-counter")
	if r := tc.mustOK("get s").(map[string]any); r["a"] != float64(1) {
		t.Errorf("value changed after rejected incr: %v", r)
	}

	// Overflow surfaces the core error.
	tc.mustOK("incr big 9223372036854775807")
	tc.mustFail("incr big", "overflows int64")
}

func TestIncrRequiresAuth(t *testing.T) {
	srv := startServer(t, testSchema(t), authPassword("s3cret"), Config{})
	tc := dial(t, srv)

	m := tc.cmd("incr c")
	if m["ok"] != false {
		t.Fatalf("pre-auth incr unexpectedly succeeded: %v", m)
	}
	tc.mustOK("auth s3cret")
	if r := tc.mustOK("incr c"); r != float64(1) {
		t.Errorf("post-auth incr = %v, want 1", r)
	}
}
