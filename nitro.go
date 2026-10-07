package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// maxEntrySize caps a single decompressed .nitro entry so a corrupt or
// hostile file cannot exhaust memory.
const maxEntrySize = 512 << 20

// nitroFile is one decompressed file stored inside a .nitro bundle.
type nitroFile struct {
	Name        string
	Data        []byte
	StoredLen   int    // compressed size inside the .nitro
	Compression string // "zlib" or "gzip"
	// Zlib is the original stream when it is zlib and ends exactly at the
	// end of the entry, so it can be stored in a .hab as-is.
	Zlib []byte
}

// readNitro parses a .nitro bundle (big-endian):
// u16 fileCount, then per file u16 nameLen, name, u32 dataLen, data,
// where data is zlib-compressed (or gzip, which some converters wrote).
func readNitro(b []byte) ([]nitroFile, error) {
	r := &beReader{b: b}
	count, err := r.u16("file count")
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, errors.New("nitro bundle contains no files")
	}
	files := make([]nitroFile, 0, count)
	seen := make(map[string]bool, count)
	for i := 0; i < int(count); i++ {
		nameLen, err := r.u16(fmt.Sprintf("file %d name length", i))
		if err != nil {
			return nil, err
		}
		nameBytes, err := r.bytes(int(nameLen), fmt.Sprintf("file %d name", i))
		if err != nil {
			return nil, err
		}
		name := string(nameBytes)
		if name == "" {
			return nil, fmt.Errorf("file %d has an empty name", i)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate file %q", name)
		}
		seen[name] = true
		dataLen, err := r.u32(fmt.Sprintf("%q data length", name))
		if err != nil {
			return nil, err
		}
		stored, err := r.bytes(int(dataLen), fmt.Sprintf("%q data", name))
		if err != nil {
			return nil, err
		}
		data, kind, exact, err := decompressNitroEntry(stored)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", name, err)
		}
		f := nitroFile{Name: name, Data: data, StoredLen: len(stored), Compression: kind}
		if kind == "zlib" && exact {
			f.Zlib = stored
		}
		files = append(files, f)
	}
	if rest := len(b) - r.off; rest != 0 {
		return nil, fmt.Errorf("%d unexpected trailing bytes after %d files", rest, count)
	}
	return files, nil
}

// decompressNitroEntry inflates a zlib or gzip entry. exact reports that the
// stream (checksum included) consumed every stored byte.
func decompressNitroEntry(stored []byte) (data []byte, kind string, exact bool, err error) {
	src := bytes.NewReader(stored) // an io.ByteReader, so nothing is read ahead
	var rc io.ReadCloser
	if len(stored) >= 2 && stored[0] == 0x1f && stored[1] == 0x8b {
		kind = "gzip"
		rc, err = gzip.NewReader(src)
	} else {
		kind = "zlib"
		rc, err = zlib.NewReader(src)
	}
	if err != nil {
		return nil, kind, false, fmt.Errorf("invalid %s data: %w", kind, err)
	}
	defer rc.Close()
	data, err = io.ReadAll(io.LimitReader(rc, maxEntrySize+1))
	if err != nil {
		return nil, kind, false, fmt.Errorf("invalid %s data: %w", kind, err)
	}
	if len(data) > maxEntrySize {
		return nil, kind, false, fmt.Errorf("decompressed size exceeds %d bytes", maxEntrySize)
	}
	return data, kind, src.Len() == 0, nil
}

type beReader struct {
	b   []byte
	off int
}

func (r *beReader) bytes(n int, what string) ([]byte, error) {
	if n < 0 || len(r.b)-r.off < n {
		return nil, fmt.Errorf("truncated nitro bundle: need %d bytes for %s at offset %d, only %d left", n, what, r.off, len(r.b)-r.off)
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s, nil
}

func (r *beReader) u16(what string) (uint16, error) {
	s, err := r.bytes(2, what)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(s), nil
}

func (r *beReader) u32(what string) (uint32, error) {
	s, err := r.bytes(4, what)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(s), nil
}
