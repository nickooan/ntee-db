package main

import (
	"fmt"
	"strings"
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

func TestTopupTakeCommands(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	// 98 + 5 vs max 100: fills to 100, 3 units overflow.
	tc.mustOK("incr c 98")
	if r := tc.mustOK("topup c 5 100"); r != float64(3) {
		t.Errorf("topup 5 at 98 = %v, want 3", r)
	}
	if r := tc.mustOK("incr c 0"); r != float64(100) {
		t.Errorf("value after partial topup = %v, want 100", r)
	}
	// At max: nothing added, the whole amount overflows.
	if r := tc.mustOK("topup c 5 100"); r != float64(5) {
		t.Errorf("topup at max = %v, want 5", r)
	}
	// 90 + 10 == 100: exact fit, overflow 0.
	tc.mustOK("decr c 10")
	if r := tc.mustOK("topup c 10 100"); r != float64(0) {
		t.Errorf("topup at 90 = %v, want 0", r)
	}
	if r := tc.mustOK("incr c 0"); r != float64(100) {
		t.Errorf("value after topup = %v, want 100", r)
	}
	// Drain to 0, then refuse below left.
	if r := tc.mustOK("take c 100 0"); r != true {
		t.Errorf("take 100 at 100 = %v, want true", r)
	}
	if r := tc.mustOK("take c 1 0"); r != false {
		t.Errorf("take at 0 = %v, want false", r)
	}
	// 11 - 10 = 1 >= 0: applies, remainder 1.
	tc.mustOK("incr c 11")
	if r := tc.mustOK("take c 10 0"); r != true {
		t.Errorf("take 10 at 11 = %v, want true", r)
	}
	if r := tc.mustOK("incr c 0"); r != float64(1) {
		t.Errorf("value after take = %v, want 1", r)
	}

	// Missing key counts as 0: a topup that adds creates it (clamped at max),
	// a refused take doesn't.
	if r := tc.mustOK("topup fresh 5 10"); r != float64(0) {
		t.Errorf("topup missing = %v, want 0", r)
	}
	if r := tc.mustOK("incr fresh 0"); r != float64(5) {
		t.Errorf("created value = %v, want 5", r)
	}
	if r := tc.mustOK("topup clamped 20 10"); r != float64(10) {
		t.Errorf("clamped topup missing = %v, want 10", r)
	}
	if r := tc.mustOK("incr clamped 0"); r != float64(10) {
		t.Errorf("clamped value = %v, want 10", r)
	}
	if r := tc.mustOK("take ghost 1 0"); r != false {
		t.Errorf("take missing = %v, want false", r)
	}
	if r := tc.mustOK("has ghost"); r != false {
		t.Error("refused take created the key")
	}
}

func TestTopupTakeArgErrors(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	tc.mustFail("topup c", "usage: topup <pk> <amount> <max>")
	tc.mustFail("topup c 1", "usage: topup <pk> <amount> <max>")
	tc.mustFail("topup c 1 2 3", "usage: topup <pk> <amount> <max>")
	tc.mustFail("take c 1", "usage: take <pk> <amount> <left>")
	tc.mustFail("topup c 1.5 10", "amount must be an integer")
	tc.mustFail("take c abc 0", "amount must be an integer")
	tc.mustFail("topup c 1 x", "max must be an integer")
	tc.mustFail("take c 1 x", "left must be an integer")
	tc.mustFail("topup c -1 10", "non-negative")
	tc.mustFail("take c -1 0", "non-negative")

	// Type rule: guarded ops on a non-counter value fail and leave it untouched.
	tc.mustOK(`put s {"a":1}`)
	tc.mustFail("topup s 1 10", "non-counter")
	tc.mustFail("take s 1 0", "non-counter")
	if r := tc.mustOK("get s").(map[string]any); r["a"] != float64(1) {
		t.Errorf("value changed after rejected ops: %v", r)
	}
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

// Explicit index values require the record value to be a JSON object —
// immediate values are primary-key-only.
func TestPutxRejectsImmediateValue(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	ix, value := `{"traceId":"T1"}`, `just-a-string`
	tc.raw(fmt.Sprintf("putx k1 %d %d\r\n%s\r\n%s\r\n", len(ix), len(value), ix, value))
	m := tc.readResp()
	if m["ok"] != false {
		t.Fatalf("putx with immediate value unexpectedly succeeded: %v", m)
	}
	if s, _ := m["err"].(string); !strings.Contains(s, "immediate value") {
		t.Errorf("err = %q, want mention of immediate value", s)
	}
	if r := tc.mustOK("has k1"); r != false {
		t.Error("rejected putx must write nothing")
	}
}
