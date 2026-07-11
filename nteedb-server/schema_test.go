package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	nteedb "github.com/nickooan/ntee-db/nteedb-core"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSchema(t *testing.T) {
	path := writeTemp(t, "schema.json", `{
		"dir": "/tmp/store",
		"blobThreshold": 65536,
		"syncEveryWrite": true,
		"hintEveryN": 100,
		"indexes": [
			{ "name": "traceId", "kind": "string" },
			{ "name": "kind", "kind": "string", "jsonPath": "kind" },
			{ "name": "status", "kind": "number", "maxPerValue": 5 }
		]
	}`)
	s, err := LoadSchema(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Dir != "/tmp/store" || !s.SyncEveryWrite || s.HintEveryN != 100 || len(s.Indexes) != 3 {
		t.Fatalf("bad schema: %+v", s)
	}

	opts, err := s.Options()
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Indexes) != 3 {
		t.Fatalf("want 3 index defs, got %d", len(opts.Indexes))
	}
	if opts.Indexes[0].Extract != nil {
		t.Error("explicit index should have no Extract")
	}
	if opts.Indexes[1].Extract == nil {
		t.Error("jsonPath index should have an Extract")
	}
	if opts.Indexes[2].MaxPerValue != 5 {
		t.Errorf("maxPerValue not carried: %+v", opts.Indexes[2])
	}

	kinds := s.Kinds()
	if kinds["status"] != nteedb.KindNumber || kinds["traceId"] != nteedb.KindString {
		t.Errorf("bad kinds map: %v", kinds)
	}
}

func TestLoadSchemaRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"bad kind":      `{"indexes":[{"name":"a","kind":"bool"}]}`,
		"empty name":    `{"indexes":[{"name":"","kind":"string"}]}`,
		"unknown field": `{"direcotry":"/tmp/x"}`,
		"not json":      `dir=/tmp`,
	}
	for name, content := range cases {
		if _, err := LoadSchema(writeTemp(t, "s.json", content)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := LoadSchema(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file: expected error")
	}
}

func TestAutoCompactConfigForms(t *testing.T) {
	load := func(t *testing.T, autoCompact string) *Schema {
		t.Helper()
		s, err := LoadSchema(writeTemp(t, "s.json", `{"dir":"/tmp/x","autoCompact":`+autoCompact+`}`))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Bool forms (backward compatible).
	if s := load(t, "true"); !s.AutoCompact.Enabled {
		t.Error("bool true: should be enabled")
	}
	if s := load(t, "false"); s.AutoCompact.Enabled {
		t.Error("bool false: should be disabled")
	}

	// Object form implies enabled; set fields land, unset stay nil.
	s := load(t, `{"mainRatio":0.3,"blobsRelieve":false,"blobMinRelieveDataSize":1024}`)
	ac := s.AutoCompact
	if !ac.Enabled || *ac.MainRatio != 0.3 || *ac.BlobsRelieve != false || *ac.BlobMinRelieveDataSize != 1024 {
		t.Errorf("object form: %+v", ac)
	}
	if ac.MainMinBytes != nil || ac.BlobRatio != nil || ac.IntervalSeconds != nil {
		t.Errorf("unset fields must stay nil: %+v", ac)
	}

	// Explicit enabled:false wins over the object-implies-enabled rule.
	if s := load(t, `{"enabled":false,"mainRatio":0.3}`); s.AutoCompact.Enabled {
		t.Error("enabled:false object: should be disabled")
	}

	// Unknown fields rejected.
	if _, err := LoadSchema(writeTemp(t, "s.json", `{"autoCompact":{"mainRation":0.3}}`)); err == nil {
		t.Error("typo'd field should be rejected")
	}
}

func TestAutoCompactConfigApply(t *testing.T) {
	f, i, n, b := 0.3, 45, int64(2048), false
	ac := AutoCompactConfig{
		Enabled: true, IntervalSeconds: &i, MainRatio: &f,
		BlobsRelieve: &b, BlobMinRelieveDataSize: &n,
	}
	var cfg Config
	ac.apply(&cfg)
	if !cfg.AutoCompact || cfg.BlobsRelieve ||
		cfg.CompactInterval != 45*time.Second || cfg.MainRatio != 0.3 ||
		cfg.BlobMinRelieveDataSize != 2048 {
		t.Errorf("apply: %+v", cfg)
	}
	// Unset fields stay zero so NewServer's defaults take over.
	if cfg.MainMinBytes != 0 || cfg.BlobRatio != 0 {
		t.Errorf("unset fields must remain zero: %+v", cfg)
	}
	// Enabled with no blobsRelieve field → blob trigger defaults on.
	var cfg2 Config
	AutoCompactConfig{Enabled: true}.apply(&cfg2)
	if !cfg2.BlobsRelieve {
		t.Error("blobsRelieve should default to enabled")
	}
	// Disabled overall → blob trigger off regardless.
	var cfg3 Config
	tr := true
	AutoCompactConfig{Enabled: false, BlobsRelieve: &tr}.apply(&cfg3)
	if cfg3.AutoCompact || cfg3.BlobsRelieve {
		t.Errorf("disabled autoCompact must disable everything: %+v", cfg3)
	}
}

func TestJSONPathExtract(t *testing.T) {
	str := jsonPathExtract("meta.kind", nteedb.KindString)
	num := jsonPathExtract("status", nteedb.KindNumber)

	if v, ok := str("k", []byte(`{"meta":{"kind":"request"}}`)); !ok || v != "request" {
		t.Errorf("nested string: got %v %v", v, ok)
	}
	if v, ok := num("k", []byte(`{"status":204}`)); !ok || v != float64(204) {
		t.Errorf("number: got %v %v", v, ok)
	}
	for name, tc := range map[string]struct {
		fn    func(string, []byte) (any, bool)
		value string
	}{
		"missing path":    {str, `{"meta":{}}`},
		"wrong kind":      {str, `{"meta":{"kind":42}}`},
		"non-object step": {str, `{"meta":"flat"}`},
		"invalid json":    {str, `not json`},
		"number as text":  {num, `{"status":"204"}`},
	} {
		if _, ok := tc.fn("k", []byte(tc.value)); ok {
			t.Errorf("%s: expected no extraction", name)
		}
	}
}
