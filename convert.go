package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// conversion is the result of converting one bundle.
type conversion struct {
	Name    string
	Entries []fileEntry // the intended (decoded) entries of Out
	Out     []byte      // the written bundle
}

// convertNitro converts .nitro bytes to .hab bytes and verifies the result
// with the official reader rules. baseName is the fallback bundle name.
func convertNitro(src []byte, baseName string) (*conversion, error) {
	files, err := readNitro(src)
	if err != nil {
		return nil, err
	}
	name := ""
	entries := make([]fileEntry, len(files))
	for i, f := range files {
		data, deflated := f.Data, f.Zlib
		if inferMimeType(f.Name) == "application/json" {
			fallback := name
			if fallback == "" {
				fallback = baseName
			}
			out, jsonName, err := transformAssetJSON(f.Data, fallback)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Name, err)
			}
			if name == "" {
				name = jsonName
			}
			data = out
			if !bytes.Equal(out, f.Data) {
				deflated = nil // the original stream no longer matches
			}
		}
		entries[i] = fileEntry{Name: f.Name, Data: data, Deflated: deflated}
	}
	if name == "" {
		name = baseName
	}
	hab, err := writeHab(name, entries)
	if err != nil {
		return nil, err
	}
	if err := verifyHab(hab, name, entries); err != nil {
		return nil, fmt.Errorf("verification failed: %w", err)
	}
	return &conversion{Name: name, Entries: entries, Out: hab}, nil
}

// verifyHab re-reads a produced bundle with the official validation and
// checks that it decodes to exactly the intended entries.
func verifyHab(hab []byte, name string, want []fileEntry) error {
	b, err := readHab(hab)
	if err != nil {
		return err
	}
	if len(b.Meta) != 3 || b.Meta["format"] != "hab" || b.Name != name {
		return fmt.Errorf("index metadata %v, want format/version/name %q", b.Meta, name)
	}
	if len(b.Entries) != len(want) {
		return fmt.Errorf("index has %d entries, want %d", len(b.Entries), len(want))
	}
	for i, e := range b.Entries {
		w := want[i]
		if e.Name != w.Name || e.MimeType != inferMimeType(w.Name) {
			return fmt.Errorf("entry %d is %q (%s), want %q", i, e.Name, e.MimeType, w.Name)
		}
		if !bytes.Equal(b.Files[e.Name], w.Data) {
			return fmt.Errorf("entry %q decodes to different bytes", e.Name)
		}
	}
	return nil
}

// baseNameNoExt returns the file name without directory and extension.
func baseNameNoExt(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// writeFileAtomic writes data to a temp file in the target directory and
// renames it into place, so readers never observe a partial file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}
