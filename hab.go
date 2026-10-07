package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
)

// Layout and limits of the official Habbo Classic (v75) HAB asset bundle.
const (
	habMagic      = "HAB\x00"
	habVersion    = 1
	habFlags      = 1
	habHeaderSize = 20
	habMaxIndex   = 64 * 1024 * 1024
	habMaxEntries = 100000
)

// fileEntry is one named file to store in a bundle.
type fileEntry struct {
	Name string
	Data []byte
	// Deflated, when set, is an existing zlib stream of Data (taken from the
	// source bundle) that may be stored instead of a fresh deflate.
	Deflated []byte
}

// habEntry is an index entry. Field order is the official writer's key order.
type habEntry struct {
	Name           string `json:"name"`
	MimeType       string `json:"mimeType"`
	Offset         int64  `json:"offset"`
	StoredLength   int64  `json:"storedLength"`
	OriginalLength int64  `json:"originalLength"`
	Compression    string `json:"compression"`
}

// habIndex is the bundle index. Field order is the official writer's key order.
type habIndex struct {
	Format  string     `json:"format"`
	Version int        `json:"version"`
	Name    string     `json:"name"`
	Entries []habEntry `json:"entries"`
}

// habHeader is the decoded 20-byte header.
type habHeader struct {
	Version, Flags                               uint16
	IndexStoredLen, IndexOriginalLen, PayloadLen uint32
}

// habBundle is a parsed bundle as the official reader sees it.
type habBundle struct {
	Header  habHeader
	Name    string
	Meta    map[string]any // all non-entry index keys
	Entries []habEntry
	Files   map[string][]byte // decoded entry bytes
	// Streams holds the stored zlib stream of each "deflate" entry whose
	// stream ends exactly at the end of its stored bytes.
	Streams map[string][]byte
}

// writeHab serialises files into a .hab bundle in the official writer's
// layout: each entry is stored as zlib (the smaller of a fresh deflate and
// its existing stream) only when that is strictly smaller than the raw
// bytes, and the compact JSON index is always deflated.
func writeHab(name string, files []fileEntry) ([]byte, error) {
	if len(files) > habMaxEntries {
		return nil, fmt.Errorf("bundle has %d entries, limit is %d", len(files), habMaxEntries)
	}
	seen := make(map[string]bool, len(files))
	entries := make([]habEntry, 0, len(files))
	stored := make([][]byte, 0, len(files))
	var offset int64
	for _, f := range files {
		if f.Name == "" {
			return nil, errors.New("entry names cannot be empty")
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("duplicate entry %q", f.Name)
		}
		seen[f.Name] = true
		deflated, err := smallestDeflate(f)
		if err != nil {
			return nil, fmt.Errorf("deflate %q: %w", f.Name, err)
		}
		data, compression := f.Data, "none"
		if len(deflated) < len(f.Data) {
			data, compression = deflated, "deflate"
		}
		entries = append(entries, habEntry{
			Name:           f.Name,
			MimeType:       inferMimeType(f.Name),
			Offset:         offset,
			StoredLength:   int64(len(data)),
			OriginalLength: int64(len(f.Data)),
			Compression:    compression,
		})
		stored = append(stored, data)
		offset += int64(len(data))
	}
	if offset > math.MaxUint32 {
		return nil, fmt.Errorf("payload of %d bytes does not fit the 32-bit header", offset)
	}

	index, err := marshalJSON(habIndex{Format: "hab", Version: habVersion, Name: name, Entries: entries})
	if err != nil {
		return nil, err
	}
	if len(index) > habMaxIndex {
		return nil, fmt.Errorf("index is %d bytes, limit is %d", len(index), habMaxIndex)
	}
	indexDeflated, err := deflate(index)
	if err != nil {
		return nil, fmt.Errorf("deflate index: %w", err)
	}

	out := make([]byte, habHeaderSize, habHeaderSize+len(indexDeflated)+int(offset))
	copy(out, habMagic)
	binary.LittleEndian.PutUint16(out[4:], habVersion)
	binary.LittleEndian.PutUint16(out[6:], habFlags)
	binary.LittleEndian.PutUint32(out[8:], uint32(len(indexDeflated)))
	binary.LittleEndian.PutUint32(out[12:], uint32(len(index)))
	binary.LittleEndian.PutUint32(out[16:], uint32(offset))
	out = append(out, indexDeflated...)
	for _, s := range stored {
		out = append(out, s...)
	}
	return out, nil
}

// smallestDeflate returns the shorter of a fresh zlib deflate and the
// entry's existing zlib stream. PNGs with an existing stream skip the fresh
// deflate: on the Nitro corpus it never won and it is most of the CPU time.
func smallestDeflate(f fileEntry) ([]byte, error) {
	if f.Deflated != nil && inferMimeType(f.Name) == "image/png" {
		return f.Deflated, nil
	}
	fresh, err := deflate(f.Data)
	if err != nil {
		return nil, err
	}
	if f.Deflated != nil && len(f.Deflated) < len(fresh) {
		return f.Deflated, nil
	}
	return fresh, nil
}

// readHab parses and validates a bundle with the same checks, in the same
// order, as the official reader's from() and validateIndex().
func readHab(b []byte) (*habBundle, error) {
	if len(b) < habHeaderSize {
		return nil, errors.New("HAB asset bundle is shorter than its header")
	}
	if string(b[:4]) != habMagic {
		return nil, errors.New("HAB asset bundle has an invalid magic signature")
	}
	h := habHeader{
		Version:          binary.LittleEndian.Uint16(b[4:]),
		Flags:            binary.LittleEndian.Uint16(b[6:]),
		IndexStoredLen:   binary.LittleEndian.Uint32(b[8:]),
		IndexOriginalLen: binary.LittleEndian.Uint32(b[12:]),
		PayloadLen:       binary.LittleEndian.Uint32(b[16:]),
	}
	if h.Version != habVersion {
		return nil, fmt.Errorf("unsupported HAB asset bundle version %d", h.Version)
	}
	if h.Flags != habFlags {
		return nil, fmt.Errorf("unsupported HAB asset bundle flags 0x%x", h.Flags)
	}
	if h.IndexOriginalLen > habMaxIndex {
		return nil, fmt.Errorf("HAB asset bundle index exceeds the %d-byte safety limit", habMaxIndex)
	}
	indexEnd := uint64(habHeaderSize) + uint64(h.IndexStoredLen)
	total := indexEnd + uint64(h.PayloadLen)
	if total != uint64(len(b)) {
		return nil, fmt.Errorf("HAB asset bundle length mismatch: expected %d, received %d", total, len(b))
	}
	index, _, err := inflate(b[habHeaderSize:indexEnd], int64(h.IndexOriginalLen))
	if err != nil {
		return nil, fmt.Errorf("HAB asset bundle index: %w", err)
	}
	if len(index) != int(h.IndexOriginalLen) {
		return nil, fmt.Errorf("HAB asset bundle index length mismatch: expected %d, decoded %d", h.IndexOriginalLen, len(index))
	}
	dec := json.NewDecoder(bytes.NewReader(index))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to parse HAB asset bundle index: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("failed to parse HAB asset bundle index: trailing data")
	}
	entries, meta, err := validateIndex(raw, int64(h.PayloadLen))
	if err != nil {
		return nil, err
	}
	bundle := &habBundle{Header: h, Meta: meta, Entries: entries,
		Files: make(map[string][]byte, len(entries)), Streams: make(map[string][]byte)}
	if s, ok := meta["name"].(string); ok {
		bundle.Name = s
	}
	payload := b[indexEnd:]
	for _, e := range entries {
		data := payload[e.Offset : e.Offset+e.StoredLength]
		if e.Compression == "deflate" {
			stream := data
			var exact bool
			if data, exact, err = inflate(stream, e.OriginalLength); err != nil {
				return nil, fmt.Errorf("HAB entry %q: %w", e.Name, err)
			}
			if exact {
				bundle.Streams[e.Name] = stream
			}
		}
		if int64(len(data)) != e.OriginalLength {
			return nil, fmt.Errorf("HAB entry %q length mismatch: expected %d, decoded %d", e.Name, e.OriginalLength, len(data))
		}
		bundle.Files[e.Name] = data
	}
	return bundle, nil
}

// validateIndex mirrors the official validateIndex (q3r).
func validateIndex(raw any, payloadLen int64) ([]habEntry, map[string]any, error) {
	errFormat := errors.New("HAB asset bundle index has an invalid format or version")
	idx, ok := raw.(map[string]any)
	if !ok || idx["format"] != "hab" {
		return nil, nil, errFormat
	}
	if v, ok := safeInteger(idx["version"]); !ok || v != habVersion {
		return nil, nil, errFormat
	}
	list, ok := idx["entries"].([]any)
	if !ok {
		return nil, nil, errFormat
	}
	if len(list) > habMaxEntries {
		return nil, nil, fmt.Errorf("HAB asset bundle exceeds the %d-entry safety limit", habMaxEntries)
	}
	entries := make([]habEntry, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		name, nameOK := m["name"].(string)
		mime, mimeOK := m["mimeType"].(string)
		if !ok || !nameOK || name == "" || !mimeOK {
			return nil, nil, errors.New("HAB asset bundle contains an invalid entry descriptor")
		}
		if seen[name] {
			return nil, nil, fmt.Errorf("HAB asset bundle contains duplicate entry %q", name)
		}
		seen[name] = true
		compression, _ := m["compression"].(string)
		if compression != "none" && compression != "deflate" {
			return nil, nil, fmt.Errorf("HAB entry %q uses unsupported compression %v", name, m["compression"])
		}
		offset, ok1 := safeInteger(m["offset"])
		storedLen, ok2 := safeInteger(m["storedLength"])
		origLen, ok3 := safeInteger(m["originalLength"])
		if !ok1 || !ok2 || !ok3 {
			return nil, nil, fmt.Errorf("HAB entry %q contains an invalid offset or length", name)
		}
		if offset+storedLen > payloadLen {
			return nil, nil, fmt.Errorf("HAB entry %q exceeds the payload boundary", name)
		}
		entries = append(entries, habEntry{
			Name: name, MimeType: mime, Offset: offset,
			StoredLength: storedLen, OriginalLength: origLen, Compression: compression,
		})
	}
	meta := make(map[string]any, len(idx))
	for k, v := range idx {
		if k != "entries" {
			meta[k] = v
		}
	}
	return entries, meta, nil
}

// safeInteger mirrors Number.isSafeInteger(v) && v >= 0.
func safeInteger(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || f != math.Trunc(f) || f < 0 || f > 1<<53-1 {
		return 0, false
	}
	return int64(f), true
}

// inferMimeType mirrors the official inferMimeTypeFromFileName.
func inferMimeType(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".json"):
		return "application/json"
	case strings.HasSuffix(n, ".xml"):
		return "text/xml"
	case strings.HasSuffix(n, ".txt"):
		return "text/plain"
	case strings.HasSuffix(n, ".html"), strings.HasSuffix(n, ".htm"):
		return "text/html"
	case strings.HasSuffix(n, ".png"):
		return "image/png"
	case strings.HasSuffix(n, ".jpg"), strings.HasSuffix(n, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(n, ".gif"):
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}

var zlibWriters = sync.Pool{New: func() any {
	w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestCompression)
	return w
}}

// deflate produces an RFC 1950 zlib stream, what pako.inflate reads.
func deflate(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(data)/2 + 64)
	w := zlibWriters.Get().(*zlib.Writer)
	defer zlibWriters.Put(w)
	w.Reset(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// inflate decodes a zlib stream, refusing output larger than limit bytes
// beyond what is needed to detect a length mismatch. exact reports that the
// stream, checksum included, used every input byte.
func inflate(data []byte, limit int64) ([]byte, bool, error) {
	src := bytes.NewReader(data) // an io.ByteReader, so nothing is read ahead
	r, err := zlib.NewReader(src)
	if err != nil {
		return nil, false, fmt.Errorf("invalid zlib data: %w", err)
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, fmt.Errorf("invalid zlib data: %w", err)
	}
	return out, int64(len(out)) <= limit && src.Len() == 0, nil
}

// marshalJSON encodes like JSON.stringify: compact, no HTML escaping.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
