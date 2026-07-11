package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

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

	// AutoCompact configures the server's background reclamation. Default off
	// (omitted field): compaction happens only via the manual admin commands.
	AutoCompact AutoCompactConfig `json:"autoCompact"`
}

// AutoCompactConfig is schema.json's "autoCompact" value — either a plain
// bool ("autoCompact": true → enabled with all defaults) or an options
// object tuning the policy. Pointer fields distinguish "unset" (server
// default applies) from an explicit zero, so user-set values always win.
type AutoCompactConfig struct {
	Enabled                bool
	IntervalSeconds        *int     // check cadence (default 30)
	MainRatio              *float64 // main-log dead share triggering Compact (default 0.5)
	MainMinBytes           *int64   // main-log size floor (default 1 MiB)
	BlobsRelieve           *bool    // false disables the automatic BlobsRelieve trigger (default true)
	BlobRatio              *float64 // orphaned blob share triggering BlobsRelieve (default 0.5)
	BlobMinRelieveDataSize *int64   // blob size floor (default 64 MiB)
}

func (c *AutoCompactConfig) UnmarshalJSON(data []byte) error {
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		*c = AutoCompactConfig{Enabled: b}
		return nil
	}
	var obj struct {
		Enabled                *bool    `json:"enabled"`
		IntervalSeconds        *int     `json:"intervalSeconds"`
		MainRatio              *float64 `json:"mainRatio"`
		MainMinBytes           *int64   `json:"mainMinBytes"`
		BlobsRelieve           *bool    `json:"blobsRelieve"`
		BlobRatio              *float64 `json:"blobRatio"`
		BlobMinRelieveDataSize *int64   `json:"blobMinRelieveDataSize"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil {
		return fmt.Errorf("autoCompact: expected true/false or an options object: %w", err)
	}
	*c = AutoCompactConfig{
		// The object form implies enabled unless "enabled": false is explicit.
		Enabled:                obj.Enabled == nil || *obj.Enabled,
		IntervalSeconds:        obj.IntervalSeconds,
		MainRatio:              obj.MainRatio,
		MainMinBytes:           obj.MainMinBytes,
		BlobsRelieve:           obj.BlobsRelieve,
		BlobRatio:              obj.BlobRatio,
		BlobMinRelieveDataSize: obj.BlobMinRelieveDataSize,
	}
	return nil
}

// apply resolves the user's auto-compact settings onto a server Config:
// explicitly set values win; unset values are left for NewServer's defaults.
func (c AutoCompactConfig) apply(cfg *Config) {
	cfg.AutoCompact = c.Enabled
	cfg.BlobsRelieve = c.Enabled // blob trigger on by default whenever auto-compact is on
	if c.BlobsRelieve != nil {
		cfg.BlobsRelieve = c.Enabled && *c.BlobsRelieve
	}
	if c.IntervalSeconds != nil && *c.IntervalSeconds > 0 {
		cfg.CompactInterval = time.Duration(*c.IntervalSeconds) * time.Second
	}
	if c.MainRatio != nil {
		cfg.MainRatio = *c.MainRatio
	}
	if c.MainMinBytes != nil {
		cfg.MainMinBytes = *c.MainMinBytes
	}
	if c.BlobRatio != nil {
		cfg.BlobRatio = *c.BlobRatio
	}
	if c.BlobMinRelieveDataSize != nil {
		cfg.BlobMinRelieveDataSize = *c.BlobMinRelieveDataSize
	}
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
