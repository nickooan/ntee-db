package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	nteedb "github.com/nickooan/ntee-db/nteedb-core"
)

// testSchema declares one explicit string index, one number index, and one
// jsonPath-derived index — the shapes the protocol has to handle.
func testSchema(t *testing.T) *Schema {
	return &Schema{
		Dir: t.TempDir(),
		Indexes: []SchemaIndex{
			{Name: "traceId", Kind: "string"},
			{Name: "status", Kind: "number"},
			{Name: "kind", Kind: "string", JSONPath: "kind"},
		},
	}
}

func startServer(t *testing.T, schema *Schema, auth *authStore, cfg Config) *Server {
	t.Helper()
	opts, err := schema.Options()
	if err != nil {
		t.Fatal(err)
	}
	db, err := nteedb.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Addr = "127.0.0.1:0"
	cfg.Quiet = true
	srv := NewServer(cfg, db, auth, schema)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		srv.Close()
		db.Close()
	})
	return srv
}

type testClient struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func dial(t *testing.T, srv *Server) *testClient {
	t.Helper()
	c, err := net.DialTimeout("tcp", srv.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &testClient{t: t, c: c, r: bufio.NewReader(c)}
}

// raw writes bytes verbatim (for pipelining / data blocks).
func (tc *testClient) raw(s string) {
	tc.t.Helper()
	if _, err := tc.c.Write([]byte(s)); err != nil {
		tc.t.Fatal(err)
	}
}

// readResp reads and decodes one JSON response line.
func (tc *testClient) readResp() map[string]any {
	tc.t.Helper()
	tc.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := tc.r.ReadString('\n')
	if err != nil {
		tc.t.Fatalf("read response: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		tc.t.Fatalf("response is not JSON: %q (%v)", line, err)
	}
	return m
}

// cmd sends one command line and returns its response.
func (tc *testClient) cmd(line string) map[string]any {
	tc.t.Helper()
	tc.raw(line + "\r\n")
	return tc.readResp()
}

// mustOK asserts ok:true and returns result.
func (tc *testClient) mustOK(line string) any {
	tc.t.Helper()
	m := tc.cmd(line)
	if m["ok"] != true {
		tc.t.Fatalf("%q failed: %v", line, m)
	}
	return m["result"]
}

func (tc *testClient) mustFail(line, errContains string) {
	tc.t.Helper()
	m := tc.cmd(line)
	if m["ok"] != false {
		tc.t.Fatalf("%q unexpectedly succeeded: %v", line, m)
	}
	if s, _ := m["err"].(string); !strings.Contains(s, errContains) {
		tc.t.Fatalf("%q error %q does not contain %q", line, s, errContains)
	}
}

// putx sends a putx frame with the given index values and value.
func (tc *testClient) putx(pk, ixJSON, value string) {
	tc.t.Helper()
	tc.raw(fmt.Sprintf("putx %s %d %d\r\n%s\r\n%s\r\n", pk, len(ixJSON), len(value), ixJSON, value))
	if m := tc.readResp(); m["ok"] != true {
		tc.t.Fatalf("putx %s failed: %v", pk, m)
	}
}

func keys(result any) []string {
	arr, _ := result.([]any)
	out := make([]string, len(arr))
	for i, v := range arr {
		out[i], _ = v.(string)
	}
	return out
}

func TestSessionCommands(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	if r := tc.mustOK("ping"); r != "pong" {
		t.Errorf("ping: %v", r)
	}
	hello := tc.mustOK("hello").(map[string]any)
	if hello["server"] != "nteedb" || hello["auth"] != "none" {
		t.Errorf("hello: %v", hello)
	}
	if n := len(hello["indexes"].([]any)); n != 3 {
		t.Errorf("hello indexes: want 3, got %d", n)
	}
	tc.mustOK("quit")
	// The server closes the connection after quit.
	tc.c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := tc.r.ReadByte(); err == nil {
		t.Error("connection still open after quit")
	}
}

func TestPutGetInline(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	tc.mustOK(`put k1 {"a": 1, "s": "with spaces"}`)
	m := tc.cmd("get k1")
	if m["ok"] != true || m["found"] != true {
		t.Fatalf("get: %v", m)
	}
	if v := m["result"].(map[string]any); v["a"] != float64(1) || v["s"] != "with spaces" {
		t.Errorf("value mangled: %v", v)
	}

	m = tc.cmd("get nope")
	if m["ok"] != true || m["found"] != false || m["result"] != nil {
		t.Errorf("miss: %v", m)
	}
}

func TestPutLengthPrefixed(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	pretty := "{\n  \"multi\": \"line\",\n  \"n\": 2\n}"
	tc.raw(fmt.Sprintf("put k2 %d\r\n%s\r\n", len(pretty), pretty))
	if m := tc.readResp(); m["ok"] != true {
		t.Fatalf("length-prefixed put: %v", m)
	}
	m := tc.cmd("get k2")
	if v := m["result"].(map[string]any); v["multi"] != "line" || v["n"] != float64(2) {
		t.Errorf("value mangled: %v", v)
	}
}

func TestBinaryValueRoundTrip(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	bin := "\xff\x00\x01raw\r\nbytes"
	tc.raw(fmt.Sprintf("put b1 %d\r\n%s\r\n", len(bin), bin))
	if m := tc.readResp(); m["ok"] != true {
		t.Fatalf("binary put: %v", m)
	}
	m := tc.cmd("get b1")
	v := m["result"].(map[string]any)
	if v["bin"] != true || v["base64"] != "/wABcmF3DQpieXRlcw==" {
		t.Errorf("binary envelope: %v", v)
	}
}

func TestIndexQueries(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	tc.putx("call:1", `{"traceId":"T1","status":200}`, `{"kind":"request"}`)
	tc.putx("call:2", `{"traceId":"T1","status":404}`, `{"kind":"request"}`)
	tc.putx("call:3", `{"traceId":"T2","status":204}`, `{"kind":"response"}`)

	if got := keys(tc.mustOK("ix traceId T1")); !equalStrings(got, []string{"call:1", "call:2"}) {
		t.Errorf("ix: %v", got)
	}
	if got := keys(tc.mustOK("ix traceId T1 -1")); !equalStrings(got, []string{"call:2"}) {
		t.Errorf("ix limit: %v", got)
	}
	if r := tc.mustOK("ixh traceId T2"); r != true {
		t.Errorf("ixh: %v", r)
	}
	if got := keys(tc.mustOK("ixr status 200 299")); !equalStrings(got, []string{"call:1", "call:3"}) {
		t.Errorf("ixr: %v", got)
	}
	if got := keys(tc.mustOK("ixp traceId T")); len(got) != 3 {
		t.Errorf("ixp: %v", got)
	}
	// jsonPath-derived index picked the value out of the records.
	if got := keys(tc.mustOK("ix kind response")); !equalStrings(got, []string{"call:3"}) {
		t.Errorf("jsonPath ix: %v", got)
	}

	recs := tc.mustOK("ixrec traceId T1").([]any)
	if len(recs) != 2 {
		t.Fatalf("ixrec: %v", recs)
	}
	first := recs[0].(map[string]any)
	if first["key"] != "call:1" || first["value"].(map[string]any)["kind"] != "request" {
		t.Errorf("ixrec entry: %v", first)
	}

	tc.mustFail("ix status abc", "not a number")
	tc.mustFail("ix ghost x", "unknown index")
}

func TestGetmHasScanDelRange(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	for i := 1; i <= 5; i++ {
		tc.mustOK(fmt.Sprintf(`put api:%03d {"n":%d}`, i, i))
	}

	entries := tc.mustOK("getm api:001 nope api:003").([]any)
	if len(entries) != 3 {
		t.Fatalf("getm: %v", entries)
	}
	e0 := entries[0].(map[string]any)
	e1 := entries[1].(map[string]any)
	if e0["found"] != true || e1["found"] != false || e1["value"] != nil {
		t.Errorf("getm entries: %v %v", e0, e1)
	}

	if r := tc.mustOK("has api:002"); r != true {
		t.Errorf("has: %v", r)
	}
	if got := keys(tc.mustOK("scan api:")); len(got) != 5 {
		t.Errorf("scan: %v", got)
	}
	tc.mustOK("del api:005")
	if r := tc.mustOK("has api:005"); r != false {
		t.Errorf("del did not delete: %v", r)
	}
	if n := tc.mustOK("rml api:002"); n != float64(1) { // deletes api:001
		t.Errorf("rml: %v", n)
	}
	if n := tc.mustOK("rmg api:003"); n != float64(1) { // deletes api:004
		t.Errorf("rmg: %v", n)
	}
	if got := keys(tc.mustOK("scan")); !equalStrings(got, []string{"api:002", "api:003"}) {
		t.Errorf("survivors: %v", got)
	}
}

func TestBadInput(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	tc.mustFail("frobnicate", "unknown command")
	tc.mustFail("get", "usage")
	tc.mustFail("ix traceId", "usage")
	tc.mustFail("put onlykey", "usage")
	// The connection survives ordinary errors.
	if r := tc.mustOK("ping"); r != "pong" {
		t.Errorf("connection dead after errors: %v", r)
	}
}

func TestLineTooLongCloses(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{MaxLine: 128})
	tc := dial(t, srv)

	tc.raw("put k " + strings.Repeat("x", 500) + "\r\n")
	m := tc.readResp()
	if m["ok"] != false || !strings.Contains(m["err"].(string), "line too long") {
		t.Fatalf("want line-too-long error, got %v", m)
	}
	tc.c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := tc.r.ReadByte(); err == nil {
		t.Error("connection should be closed after oversized line")
	}
}

func TestAuthPasswordFlow(t *testing.T) {
	srv := startServer(t, testSchema(t), authPassword("hunter2"), Config{})
	tc := dial(t, srv)

	tc.mustFail("get k", "auth required")
	if r := tc.mustOK("ping"); r != "pong" { // ping allowed pre-auth
		t.Errorf("ping pre-auth: %v", r)
	}
	tc.mustFail("auth wrong", "invalid password")
	tc.mustFail("get k", "auth required") // still unauthenticated
	tc.mustOK("auth hunter2")
	m := tc.cmd("get k")
	if m["ok"] != true {
		t.Errorf("get after auth: %v", m)
	}
	tc.mustOK("compact") // password mode grants admin
}

func TestAuthRoles(t *testing.T) {
	path := writeTemp(t, "users.txt", "app:apppw\nops:opspw:admin\n")
	auth, err := authFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := startServer(t, testSchema(t), auth, Config{})

	app := dial(t, srv)
	app.mustOK("auth app apppw")
	app.mustOK(`put k1 {"a":1}`) // data commands allowed
	if r := app.mustOK("stats").(map[string]any); r["records"] != float64(1) {
		t.Errorf("stats for user role: %v", r)
	}
	app.mustFail("compact", "requires admin")
	app.mustFail("reindex", "requires admin")
	app.mustFail("relieve", "requires admin")

	ops := dial(t, srv)
	ops.mustOK("auth ops opspw")
	ops.mustOK("compact")
	ops.mustOK("reindex")
	ops.mustOK("relieve")
}

func TestRelieveCommand(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	// The manual relieve command is unconditional (no thresholds): it rewrites
	// blobs and compacts the main log even on a tiny store.
	for round := 0; round < 2; round++ {
		for i := 0; i < 20; i++ {
			tc.mustOK(fmt.Sprintf(`put k:%02d {"round":%d}`, i, round))
		}
	}
	before := tc.mustOK("stats").(map[string]any)

	if r := tc.mustOK("relieve"); r != true {
		t.Fatalf("relieve result: %v", r)
	}
	after := tc.mustOK("stats").(map[string]any)
	if after["mainBytes"].(float64) >= before["mainBytes"].(float64) {
		t.Errorf("relieve did not compact main log: %v → %v", before["mainBytes"], after["mainBytes"])
	}
	if after["blobCompacts"].(float64) != 1 {
		t.Errorf("blobCompacts should be 1, got %v", after["blobCompacts"])
	}
	m := tc.cmd("get k:05")
	if m["found"] != true {
		t.Fatalf("data lost after relieve: %v", m)
	}
}

// TestAutoCompactBlobTrigger drives the policy's blob branch deterministically:
// tiny thresholds + blob-backed values, then a direct maybeCompact call.
func TestAutoCompactBlobTrigger(t *testing.T) {
	schema := testSchema(t)
	schema.AutoCompact = AutoCompactConfig{Enabled: true}
	schema.BlobThreshold = 64 // values ≥64 bytes become blobs
	srv := startServer(t, schema, authNone(), Config{
		AutoCompact:            true,
		CompactInterval:        time.Hour, // loop never ticks; we call maybeCompact directly
		MainMinBytes:           1,
		BlobsRelieve:           true,
		BlobMinRelieveDataSize: 1,
		BlobRatio:              0.5,
	})
	tc := dial(t, srv)

	// Two big values under one key: the first becomes an orphaned blob (50%).
	big := fmt.Sprintf(`{"pad":%q}`, strings.Repeat("x", 100))
	tc.raw(fmt.Sprintf("put big:1 %d\r\n%s\r\n", len(big), big))
	tc.readResp()
	tc.raw(fmt.Sprintf("put big:1 %d\r\n%s\r\n", len(big), big))
	tc.readResp()
	before := tc.mustOK("stats").(map[string]any)
	if before["blobBytes"].(float64) != float64(2*len(big)) {
		t.Fatalf("expected 2 blobs on disk: %v", before)
	}

	srv.maybeCompact()

	after := tc.mustOK("stats").(map[string]any)
	if after["blobCompacts"].(float64) != 1 || after["autoCompacts"].(float64) != 1 {
		t.Fatalf("blob trigger did not fire: %v", after)
	}
	if after["blobBytes"].(float64) != float64(len(big)) {
		t.Errorf("orphaned blob not reclaimed: %v", after["blobBytes"])
	}
	m := tc.cmd("get big:1")
	if m["found"] != true {
		t.Fatalf("blob value lost: %v", m)
	}
}

// With the blob trigger disabled, orphaned blobs are ignored and only the
// main-log branch runs.
func TestAutoCompactBlobTriggerDisabled(t *testing.T) {
	schema := testSchema(t)
	schema.AutoCompact = AutoCompactConfig{Enabled: true}
	schema.BlobThreshold = 64
	srv := startServer(t, schema, authNone(), Config{
		AutoCompact:            true,
		CompactInterval:        time.Hour,
		MainMinBytes:           1,
		BlobsRelieve:           false, // the toggle under test
		BlobMinRelieveDataSize: 1,
		BlobRatio:              0.5,
	})
	tc := dial(t, srv)

	// Three writes of one key: 2 orphaned blobs (67%) and a main-log dead
	// ratio of ~2/3 — both branches' thresholds are crossed.
	big := fmt.Sprintf(`{"pad":%q}`, strings.Repeat("x", 100))
	for i := 0; i < 3; i++ {
		tc.raw(fmt.Sprintf("put big:1 %d\r\n%s\r\n", len(big), big))
		tc.readResp()
	}

	srv.maybeCompact()

	after := tc.mustOK("stats").(map[string]any)
	if after["blobCompacts"].(float64) != 0 {
		t.Fatalf("blob trigger fired despite being disabled: %v", after)
	}
	if after["autoCompacts"].(float64) != 1 {
		t.Fatalf("main-log branch should still have compacted: %v", after)
	}
	if after["blobBytes"].(float64) != float64(3*len(big)) {
		t.Errorf("blob file must be untouched: %v", after["blobBytes"])
	}
	if after["mainBytes"] != after["liveBytes"] {
		t.Errorf("main log not reclaimed: %v", after)
	}
}

func TestParallelReads(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	seed := dial(t, srv)
	for i := 0; i < 50; i++ {
		seed.mustOK(fmt.Sprintf(`put k:%02d {"n":%d}`, i, i))
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", srv.Addr(), time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			r := bufio.NewReader(c)
			for i := 0; i < 50; i++ {
				fmt.Fprintf(c, "get k:%02d\r\n", i)
				line, err := r.ReadString('\n')
				if err != nil {
					errs <- err
					return
				}
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil || m["found"] != true {
					errs <- fmt.Errorf("bad response %q (%v)", line, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPipelining(t *testing.T) {
	srv := startServer(t, testSchema(t), authNone(), Config{})
	tc := dial(t, srv)

	// Three commands in a single write; three responses in order.
	tc.raw("ping\r\nput p1 {\"x\":1}\r\nget p1\r\n")
	if m := tc.readResp(); m["result"] != "pong" {
		t.Fatalf("resp 1: %v", m)
	}
	if m := tc.readResp(); m["ok"] != true {
		t.Fatalf("resp 2: %v", m)
	}
	if m := tc.readResp(); m["found"] != true {
		t.Fatalf("resp 3: %v", m)
	}
}

func TestAutoCompact(t *testing.T) {
	// 40 records, then delete 30: the survivors' lines are a small fraction of
	// a log that also holds 30 dead records and 30 tombstones — well past the
	// 0.5 trigger.
	churn := func(tc *testClient) map[string]any {
		t.Helper()
		for i := 0; i < 40; i++ {
			tc.mustOK(fmt.Sprintf(`put k:%02d {"n":%d}`, i, i))
		}
		for i := 10; i < 40; i++ {
			tc.mustOK(fmt.Sprintf("del k:%02d", i))
		}
		before := tc.mustOK("stats").(map[string]any)
		if before["mainBytes"].(float64) <= before["liveBytes"].(float64) {
			t.Fatalf("expected dead space before compaction: %v", before)
		}
		return before
	}

	// Deterministic policy check: the loop is running but its interval is far
	// beyond the test (it can never fire mid-churn); trigger the check
	// directly once the store is quiescent.
	t.Run("policy", func(t *testing.T) {
		schema := testSchema(t)
		schema.AutoCompact = AutoCompactConfig{Enabled: true}
		srv := startServer(t, schema, authNone(), Config{
			AutoCompact:     true,
			CompactInterval: time.Hour,
			MainMinBytes:    1,
			MainRatio:       0.5,
		})
		tc := dial(t, srv)
		before := churn(tc)

		srv.maybeCompact()

		after := tc.mustOK("stats").(map[string]any)
		if after["autoCompacts"].(float64) != 1 {
			t.Fatalf("expected exactly one auto-compact: %v", after)
		}
		if after["blobCompacts"].(float64) != 0 {
			t.Fatalf("blob-free store must take the Compact branch: %v", after)
		}
		if after["mainBytes"] != after["liveBytes"] {
			t.Fatalf("log not fully reclaimed: %v", after)
		}
		if after["mainBytes"].(float64) >= before["mainBytes"].(float64) {
			t.Fatalf("log did not shrink: before %v after %v", before["mainBytes"], after["mainBytes"])
		}
		// Store contents intact after compaction.
		if got := keys(tc.mustOK("scan k:")); len(got) != 10 {
			t.Fatalf("survivors after compact: %v", got)
		}
		m := tc.cmd("get k:05")
		if m["found"] != true || m["result"].(map[string]any)["n"] != float64(5) {
			t.Fatalf("read after compact: %v", m)
		}
	})

	// Liveness check: with a real (fast) interval the background goroutine
	// fires on its own. A tick may land mid-churn and leave residual dead
	// space below the trigger, so assert only that it ran — the policy
	// subtest covers the rest.
	t.Run("loop", func(t *testing.T) {
		schema := testSchema(t)
		schema.AutoCompact = AutoCompactConfig{Enabled: true}
		srv := startServer(t, schema, authNone(), Config{
			AutoCompact:     true,
			CompactInterval: 20 * time.Millisecond,
			MainMinBytes:    1,
			MainRatio:       0.5,
		})
		tc := dial(t, srv)
		churn(tc)

		deadline := time.Now().Add(3 * time.Second)
		for {
			after := tc.mustOK("stats").(map[string]any)
			if after["autoCompacts"].(float64) >= 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("auto-compact loop never fired: %v", after)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

func TestProtectedMode(t *testing.T) {
	none, pw := authNone(), authPassword("x")
	for _, tc := range []struct {
		addr     string
		auth     *authStore
		insecure bool
		wantErr  bool
	}{
		{"127.0.0.1:6740", none, false, false},
		{"localhost:6740", none, false, false},
		{"0.0.0.0:6740", none, false, true},
		{"192.168.1.5:6740", none, false, true},
		{"0.0.0.0:6740", pw, false, false},
		{"0.0.0.0:6740", none, true, false},
		{"no-port", none, false, true},
	} {
		err := checkProtectedMode(tc.addr, tc.auth, tc.insecure)
		if (err != nil) != tc.wantErr {
			t.Errorf("checkProtectedMode(%q, %s, insecure=%v): %v, wantErr=%v",
				tc.addr, tc.auth.mode, tc.insecure, err, tc.wantErr)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
