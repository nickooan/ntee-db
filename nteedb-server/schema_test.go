package main

import (
	"os"
	"path/filepath"
	"testing"

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
