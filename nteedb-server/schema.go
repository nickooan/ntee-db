package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	nteedb "github.com/nickooan/ntee-db/nteedb-core"
)

// Schema is the server's startup description of the store — the same shape the
// JS binding takes in NteeDB.open (dir, blobThreshold, syncEveryWrite,
// hintEveryN, indexes with name/kind/jsonPath/maxPerValue).
type Schema struct {
	Dir            string        `json:"dir"`
	BlobThreshold  int           `json:"blobThreshold"`
	SyncEveryWrite bool          `json:"syncEveryWrite"`
	HintEveryN     int           `json:"hintEveryN"`
	Indexes        []SchemaIndex `json:"indexes"`
}

type SchemaIndex struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"` // "string" | "number"
	JSONPath    string `json:"jsonPath,omitempty"`
	MaxPerValue int    `json:"maxPerValue,omitempty"`
}

func LoadSchema(path string) (*Schema, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	var s Schema
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse schema %s: %w", path, err)
	}
	for _, ix := range s.Indexes {
		if ix.Name == "" {
			return nil, fmt.Errorf("schema: index with empty name")
		}
		if _, err := parseKind(ix.Kind); err != nil {
			return nil, fmt.Errorf("schema: index %q: %w", ix.Name, err)
		}
	}
	return &s, nil
}

func parseKind(kind string) (nteedb.ValueKind, error) {
	switch kind {
	case "string":
		return nteedb.KindString, nil
	case "number":
		return nteedb.KindNumber, nil
	default:
		return 0, fmt.Errorf("unknown kind %q (expected \"string\" or \"number\")", kind)
	}
}

// Options converts the schema into core Options. jsonPath indexes get an
// Extract func compiled from the dotted path.
func (s *Schema) Options() (nteedb.Options, error) {
	opts := nteedb.Options{
		Dir:            s.Dir,
		BlobThreshold:  s.BlobThreshold,
		SyncEveryWrite: s.SyncEveryWrite,
		HintEveryN:     s.HintEveryN,
	}
	for _, ix := range s.Indexes {
		kind, err := parseKind(ix.Kind)
		if err != nil {
			return opts, err
		}
		def := nteedb.IndexDef{Name: ix.Name, Kind: kind, MaxPerValue: ix.MaxPerValue}
		if ix.JSONPath != "" {
			def.Extract = jsonPathExtract(ix.JSONPath, kind)
		}
		opts.Indexes = append(opts.Indexes, def)
	}
	return opts, nil
}

// Kinds returns name → kind for the declared indexes, used to parse index
// values arriving as protocol text tokens.
func (s *Schema) Kinds() map[string]nteedb.ValueKind {
	m := make(map[string]nteedb.ValueKind, len(s.Indexes))
	for _, ix := range s.Indexes {
		k, _ := parseKind(ix.Kind) // validated at load
		m[ix.Name] = k
	}
	return m
}

// jsonPathExtract compiles a dotted path ("a.b.c") into an IndexDef.Extract:
// decode the record, walk the path, and accept only a value matching the
// declared kind — same semantics the JS binding documents for jsonPath.
func jsonPathExtract(path string, kind nteedb.ValueKind) func(key string, value []byte) (any, bool) {
	segs := strings.Split(path, ".")
	return func(_ string, value []byte) (any, bool) {
		var v any
		if err := json.Unmarshal(value, &v); err != nil {
			return nil, false
		}
		for _, seg := range segs {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, false
			}
			if v, ok = m[seg]; !ok {
				return nil, false
			}
		}
		switch kind {
		case nteedb.KindString:
			if s, ok := v.(string); ok {
				return s, true
			}
		case nteedb.KindNumber:
			if f, ok := v.(float64); ok {
				return f, true
			}
		}
		return nil, false
	}
}
