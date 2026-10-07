package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Octane's BinaryReader reads the .nitro counts and lengths as signed
// big-endian integers, so values must stay within the positive range.
const (
	nitroMaxCount   = math.MaxInt16
	nitroMaxNameLen = math.MaxInt16
	nitroMaxDataLen = math.MaxInt32
)

var errNotJSONBundle = errors.New("not a JSON asset bundle")

// imageMimeTypes are the entry types a Nitro bundle can carry besides JSON.
var imageMimeTypes = []string{"image/png", "image/gif", "image/jpeg"}

// convertHab converts .hab bytes to .nitro bytes and verifies the result
// with the nitro reader. Only JSON asset bundles (JSON plus images) are
// supported; baseName is the fallback name for documentClass matching.
func convertHab(src []byte, baseName string) (*conversion, error) {
	b, err := readHab(src)
	if err != nil {
		return nil, err
	}
	if err := checkJSONAssetBundle(b); err != nil {
		return nil, err
	}
	name := b.Name
	if name == "" {
		name = baseName
	}
	entries := make([]fileEntry, len(b.Entries))
	for i, e := range b.Entries {
		data, stream := b.Files[e.Name], b.Streams[e.Name]
		if e.MimeType == "application/json" {
			out, err := stripDocumentClass(data, name)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name, err)
			}
			if !bytes.Equal(out, data) {
				stream = nil // the stored stream no longer matches
			}
			data = out
		}
		entries[i] = fileEntry{Name: e.Name, Data: data, Deflated: stream}
	}
	nitro, err := writeNitro(entries)
	if err != nil {
		return nil, err
	}
	if err := verifyNitro(nitro, entries); err != nil {
		return nil, fmt.Errorf("verification failed: %w", err)
	}
	return &conversion{Name: name, Entries: entries, Out: nitro}, nil
}

// checkJSONAssetBundle accepts only what the Nitro renderer can load: plain
// format/version/name metadata, at least one JSON entry (named *.json, which
// is how the renderer recognises it) and images for everything else.
func checkJSONAssetBundle(b *habBundle) error {
	for k := range b.Meta {
		if k != "format" && k != "version" && k != "name" {
			return fmt.Errorf("%w: index has %q metadata (an official XML library?)", errNotJSONBundle, k)
		}
	}
	jsonEntries := 0
	for _, e := range b.Entries {
		isJSONName := strings.HasSuffix(e.Name, ".json")
		switch {
		case e.MimeType == "application/json" && isJSONName:
			jsonEntries++
		case slices.Contains(imageMimeTypes, e.MimeType) && !isJSONName:
		default:
			return fmt.Errorf("%w: entry %q has type %s (only *.json JSON and png/gif/jpeg images are supported)", errNotJSONBundle, e.Name, e.MimeType)
		}
	}
	if jsonEntries == 0 {
		return fmt.Errorf("%w: it has no JSON entry", errNotJSONBundle)
	}
	return nil
}

// writeNitro serialises entries as a .nitro bundle (big-endian u16 count,
// then per entry u16 name length, name, u32 data length, zlib stream).
// Every entry is zlib-compressed because the renderer inflates them all;
// an entry's existing stream is used when it has one.
func writeNitro(entries []fileEntry) ([]byte, error) {
	if len(entries) == 0 || len(entries) > nitroMaxCount {
		return nil, fmt.Errorf("a nitro bundle holds 1 to %d files, got %d", nitroMaxCount, len(entries))
	}
	var out bytes.Buffer
	binary.Write(&out, binary.BigEndian, uint16(len(entries)))
	for _, e := range entries {
		if e.Name == "" || len(e.Name) > nitroMaxNameLen {
			return nil, fmt.Errorf("entry name %q must be 1 to %d bytes", e.Name, nitroMaxNameLen)
		}
		stream := e.Deflated
		if stream == nil {
			var err error
			if stream, err = deflate(e.Data); err != nil {
				return nil, fmt.Errorf("deflate %q: %w", e.Name, err)
			}
		}
		if len(stream) > nitroMaxDataLen {
			return nil, fmt.Errorf("entry %q compresses to %d bytes, limit is %d", e.Name, len(stream), nitroMaxDataLen)
		}
		binary.Write(&out, binary.BigEndian, uint16(len(e.Name)))
		out.WriteString(e.Name)
		binary.Write(&out, binary.BigEndian, uint32(len(stream)))
		out.Write(stream)
	}
	return out.Bytes(), nil
}

// verifyNitro re-reads a produced .nitro and checks that every entry is a
// zlib stream decoding to exactly the intended bytes, in order.
func verifyNitro(nitro []byte, want []fileEntry) error {
	files, err := readNitro(nitro)
	if err != nil {
		return err
	}
	if len(files) != len(want) {
		return fmt.Errorf("bundle has %d files, want %d", len(files), len(want))
	}
	for i, f := range files {
		if f.Name != want[i].Name {
			return fmt.Errorf("file %d is %q, want %q", i, f.Name, want[i].Name)
		}
		if f.Compression != "zlib" {
			return fmt.Errorf("file %q is %s, not zlib", f.Name, f.Compression)
		}
		if !bytes.Equal(f.Data, want[i].Data) {
			return fmt.Errorf("file %q decodes to different bytes", f.Name)
		}
	}
	return nil
}
