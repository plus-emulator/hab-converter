package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// jsonMember is one top-level member of a JSON object with its key kept as
// the original raw bytes and its value compacted.
type jsonMember struct {
	RawKey []byte
	Key    string
	Value  []byte
}

// transformAssetJSON returns the compact form of a Nitro asset JSON object
// with "documentClass" inserted immediately before the top-level
// "spritesheet" key (or appended when there is none), as the official .hab
// files have it. Keys keep their order and values keep their bytes; an
// existing documentClass is left alone. The class name is the object's
// "name" string, or fallbackName when that is missing or empty; it is
// returned alongside the output.
func transformAssetJSON(src []byte, fallbackName string) ([]byte, string, error) {
	members, err := splitObject(src)
	if err != nil {
		return nil, "", err
	}
	name := stringMember(members, "name")
	hasClass := false
	insertAt := len(members)
	for i, m := range members {
		switch m.Key {
		case "documentClass":
			hasClass = true
		case "spritesheet":
			if insertAt == len(members) {
				insertAt = i
			}
		}
	}
	if name == "" {
		name = fallbackName
	}
	if !hasClass {
		value, err := marshalJSON(name)
		if err != nil {
			return nil, "", err
		}
		dc := jsonMember{RawKey: []byte(`"documentClass"`), Key: "documentClass", Value: value}
		members = append(members[:insertAt], append([]jsonMember{dc}, members[insertAt:]...)...)
	}

	return joinObject(members, len(src)+len(name)+20), name, nil
}

// stripDocumentClass reverses transformAssetJSON: it returns the compact
// object without top-level "documentClass" members whose value is the
// bundle name or the object's own "name" (what .hab adds). Any other
// documentClass is kept.
func stripDocumentClass(src []byte, bundleName string) ([]byte, error) {
	members, err := splitObject(src)
	if err != nil {
		return nil, err
	}
	jsonName := stringMember(members, "name")
	kept := members[:0:0]
	for _, m := range members {
		if m.Key == "documentClass" {
			var v string
			if json.Unmarshal(m.Value, &v) == nil && v != "" && (v == bundleName || v == jsonName) {
				continue
			}
		}
		kept = append(kept, m)
	}
	return joinObject(kept, len(src)), nil
}

// stringMember returns the string value of the last member named key, or
// "" when it is missing or not a string (JSON.parse keeps the last one).
func stringMember(members []jsonMember, key string) string {
	v := ""
	for _, m := range members {
		if m.Key == key {
			if json.Unmarshal(m.Value, &v) != nil {
				v = ""
			}
		}
	}
	return v
}

// joinObject writes members back as one compact JSON object.
func joinObject(members []jsonMember, sizeHint int) []byte {
	var out bytes.Buffer
	out.Grow(sizeHint)
	out.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(m.RawKey)
		out.WriteByte(':')
		out.Write(m.Value)
	}
	out.WriteByte('}')
	return out.Bytes()
}

// splitObject streams the top-level members of a JSON object without
// decoding values, so key order and number/string bytes are preserved.
func splitObject(src []byte) ([]jsonMember, error) {
	src = bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	dec := json.NewDecoder(bytes.NewReader(src))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("JSON root is not an object")
	}
	var members []jsonMember
	for dec.More() {
		start := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("invalid JSON: object key is not a string")
		}
		rawKey := bytes.TrimLeft(src[start:dec.InputOffset()], " \t\r\n,")
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("invalid JSON value for %q: %w", key, err)
		}
		var value bytes.Buffer
		if err := json.Compact(&value, raw); err != nil {
			return nil, fmt.Errorf("invalid JSON value for %q: %w", key, err)
		}
		members = append(members, jsonMember{RawKey: bytes.Clone(rawKey), Key: key, Value: value.Bytes()})
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: trailing data after root object")
	}
	return members, nil
}
