package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const compactJSON = `{"name":"lamp","logicType":"furniture_basic","assets":{"lamp_64_a_0_0":{"x":-1,"y":33.50}},"spritesheet":{"frames":{},"meta":{"size":{"w":1,"h":1}}}}`

func TestHabToNitroRoundTrip(t *testing.T) {
	png := []byte(strings.Repeat("png-ish data ", 300))
	original := buildNitro(t,
		testFile{name: "lamp.json", data: []byte(compactJSON)},
		testFile{name: "lamp.png", data: png},
	)
	hab, err := convertNitro(original, "lamp")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := convertHab(hab.Out, "lamp")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := readNitro(original)
	got, err := readNitro(conv.Out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d files, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name || !bytes.Equal(got[i].Data, want[i].Data) || got[i].Compression != "zlib" {
			t.Errorf("file %d: %s (%s) %q, want %s %q", i, got[i].Name, got[i].Compression, got[i].Data, want[i].Name, want[i].Data)
		}
	}
}

func TestStripDocumentClass(t *testing.T) {
	tests := []struct {
		name, in, bundle, want string
	}{
		{"equal to name", `{"name":"a","documentClass":"a","spritesheet":{}}`, "a", `{"name":"a","spritesheet":{}}`},
		{"equal to bundle name only", `{"type":"fx","documentClass":"fx_1"}`, "fx_1", `{"type":"fx"}`},
		{"different value kept", `{"name":"a","documentClass":"Custom","spritesheet":{}}`, "a", `{"name":"a","documentClass":"Custom","spritesheet":{}}`},
		{"non-string kept", `{"name":"a","documentClass":1}`, "a", `{"name":"a","documentClass":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stripDocumentClass([]byte(tc.in), tc.bundle)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestHabToNitroStreamReuse(t *testing.T) {
	png := []byte(strings.Repeat("pixels ", 500))
	noise := randomBytes(800)
	kept := []byte(`{"name":"x","documentClass":"Other","pad":"` + strings.Repeat("ab", 100) + `"}`)
	hab, err := writeHab("x", []fileEntry{
		{Name: "x.json", Data: []byte(`{"name":"x","documentClass":"x"}`)},
		{Name: "keep.json", Data: kept},
		{Name: "x.png", Data: png},
		{Name: "noise.png", Data: noise},
	})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := convertHab(hab, "x")
	if err != nil {
		t.Fatal(err)
	}
	files, err := readNitro(conv.Out)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]nitroFile{}
	for _, f := range files {
		byName[f.Name] = f
	}

	// Unchanged deflated entries keep the .hab stream byte for byte.
	for _, name := range []string{"x.png", "keep.json"} {
		habStream, e := storedBytes(t, hab, name)
		if e.Compression != "deflate" || !bytes.Equal(byName[name].Zlib, habStream) {
			t.Errorf("%s: nitro stream is not the .hab stream", name)
		}
	}
	// The changed JSON and the raw-stored PNG are deflated fresh.
	if f := byName["x.json"]; string(f.Data) != `{"name":"x"}` || f.Zlib == nil {
		t.Errorf("x.json: %q (zlib %v)", f.Data, f.Zlib != nil)
	}
	if f := byName["noise.png"]; !bytes.Equal(f.Data, noise) || f.Compression != "zlib" {
		t.Errorf("noise.png: not a zlib stream of the original bytes")
	}
	if string(byName["keep.json"].Data) != string(kept) {
		t.Errorf("keep.json changed: %s", byName["keep.json"].Data)
	}
}

func TestHabToNitroRejectsNonJSONBundles(t *testing.T) {
	cases := map[string][]fileEntry{
		"xml library":     {{Name: "index", Data: []byte("<x/>")}, {Name: "room_assets.xml", Data: []byte("<x/>")}},
		"no json":         {{Name: "a.png", Data: []byte("png")}},
		"json plus other": {{Name: "a.json", Data: []byte(`{}`)}, {Name: "a.txt", Data: []byte("x")}},
	}
	for name, entries := range cases {
		hab, err := writeHab("lib", entries)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := convertHab(hab, "lib"); !errors.Is(err, errNotJSONBundle) {
			t.Errorf("%s: got %v, want %v", name, err, errNotJSONBundle)
		}
	}
	// Index metadata beyond format/version/name (official XML libraries carry a manifest).
	b := &habBundle{Meta: map[string]any{"format": "hab", "version": 1, "name": "lib", "manifest": "<manifest/>"},
		Entries: []habEntry{{Name: "a.json", MimeType: "application/json"}}}
	if err := checkJSONAssetBundle(b); !errors.Is(err, errNotJSONBundle) {
		t.Errorf("manifest metadata: got %v", err)
	}
}
