package main

import (
	"encoding/json"
	"strings"

	nteedb "codeberg.org/nickoan/ntee-r1quest/ntee-db/nteedb-core"
)

// jsonOptions mirrors nteedb.Options for the JSON passed across the FFI boundary,
// plus a declarative jsonPath per index (since function extractors can't cross
// the boundary).
type jsonOptions struct {
	Dir            string      `json:"dir"`
	BlobThreshold  int         `json:"blobThreshold"`
	SyncEveryWrite bool        `json:"syncEveryWrite"`
	HintEveryN     int         `json:"hintEveryN"`
	Indexes        []jsonIndex `json:"indexes"`
}

type jsonIndex struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`        // "string" | "number"
	JSONPath    string `json:"jsonPath"`    // optional dotted path into a JSON value
	MaxPerValue int    `json:"maxPerValue"` // cap on records per distinct value; 0 = unlimited
}

// parseOptions builds nteedb.Options from the dir + an options JSON string.
func parseOptions(dir, optsJSON string) (nteedb.Options, error) {
	opts := nteedb.Options{Dir: dir}
	if optsJSON == "" {
		return opts, nil
	}
	var jo jsonOptions
	if err := json.Unmarshal([]byte(optsJSON), &jo); err != nil {
		return opts, err
	}
	if jo.Dir != "" {
		opts.Dir = jo.Dir
	}
	opts.BlobThreshold = jo.BlobThreshold
	opts.SyncEveryWrite = jo.SyncEveryWrite
	opts.HintEveryN = jo.HintEveryN
	for _, ji := range jo.Indexes {
		kind := nteedb.KindString
		if ji.Kind == "number" {
			kind = nteedb.KindNumber
		}
		def := nteedb.IndexDef{Name: ji.Name, Kind: kind, MaxPerValue: ji.MaxPerValue}
		if ji.JSONPath != "" {
			def.Extract = jsonPathExtractor(ji.JSONPath, kind)
		}
		opts.Indexes = append(opts.Indexes, def)
	}
	return opts, nil
}

// jsonPathExtractor returns an IndexDef.Extract that pulls a dotted field path
// out of a JSON record value and returns it typed per the index kind. Objects
// only: the path cannot traverse arrays (an array element ends the walk).
func jsonPathExtractor(path string, kind nteedb.ValueKind) func(string, []byte) (any, bool) {
	parts := strings.Split(path, ".")
	return func(_ string, value []byte) (any, bool) {
		var cur any
		if json.Unmarshal(value, &cur) != nil {
			return nil, false
		}
		for _, p := range parts {
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			cur, ok = obj[p]
			if !ok {
				return nil, false
			}
		}
		if kind == nteedb.KindNumber {
			f, ok := cur.(float64)
			return f, ok
		}
		s, ok := cur.(string)
		return s, ok
	}
}
