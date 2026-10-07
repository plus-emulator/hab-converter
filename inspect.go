package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
)

// runInspect prints the header and entry table of .hab and .nitro files.
func runInspect(inputs []string, stdout, stderr io.Writer) int {
	var paths []string
	failed := 0
	for _, input := range inputs {
		info, err := os.Stat(input)
		if err != nil {
			fmt.Fprintf(stderr, "FAIL %v\n", err)
			failed++
			continue
		}
		if !info.IsDir() {
			paths = append(paths, input)
			continue
		}
		filepath.WalkDir(input, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				ext := strings.ToLower(filepath.Ext(path))
				if ext == ".hab" || ext == ".nitro" {
					paths = append(paths, path)
				}
			}
			return nil
		})
	}
	for _, path := range paths {
		if err := inspectFile(path, stdout); err != nil {
			fmt.Fprintf(stderr, "FAIL %s: %v\n", path, err)
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func inspectFile(path string, w io.Writer) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	if strings.EqualFold(filepath.Ext(path), ".nitro") {
		files, err := readNitro(b)
		if err != nil {
			return err
		}
		fmt.Fprintf(tw, "%s: nitro, %d bytes, %d files\n", path, len(b), len(files))
		fmt.Fprintf(tw, "  name\tmime\toriginal\tstored\tcompression\n")
		for _, f := range files {
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%s\n", f.Name, inferMimeType(f.Name), len(f.Data), f.StoredLen, f.Compression)
		}
		return nil
	}
	bundle, err := readHab(b)
	if err != nil {
		return err
	}
	h := bundle.Header
	fmt.Fprintf(tw, "%s: hab v%d flags 0x%x, %d bytes, index %d/%d bytes (stored/original), payload %d bytes\n",
		path, h.Version, h.Flags, len(b), h.IndexStoredLen, h.IndexOriginalLen, h.PayloadLen)
	keys := make([]string, 0, len(bundle.Meta))
	for k := range bundle.Meta {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(tw, "  %s: %v\n", k, bundle.Meta[k])
	}
	fmt.Fprintf(tw, "  name\tmime\toriginal\tstored\tcompression\n")
	for _, e := range bundle.Entries {
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%s\n", e.Name, e.MimeType, e.OriginalLength, e.StoredLength, e.Compression)
	}
	return nil
}
