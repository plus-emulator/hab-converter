package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
)

type testFile struct {
	name   string
	data   []byte
	gzip   bool
	stored []byte // when set, written verbatim instead of compressing data
}

func buildNitro(t *testing.T, files ...testFile) []byte {
	t.Helper()
	var out bytes.Buffer
	binary.Write(&out, binary.BigEndian, uint16(len(files)))
	for _, f := range files {
		var c bytes.Buffer
		var w interface {
			Write([]byte) (int, error)
			Close() error
		}
		if f.gzip {
			w = gzip.NewWriter(&c)
		} else {
			w = zlib.NewWriter(&c)
		}
		w.Write(f.data)
		w.Close()
		if f.stored != nil {
			c = *bytes.NewBuffer(f.stored)
		}
		binary.Write(&out, binary.BigEndian, uint16(len(f.name)))
		out.WriteString(f.name)
		binary.Write(&out, binary.BigEndian, uint32(c.Len()))
		out.Write(c.Bytes())
	}
	return out.Bytes()
}

func randomBytes(n int) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

const sampleJSON = `{
  "name": "chair",
  "logicType": "furniture_basic",
  "assets": {"chair_64_a_0_0": {"x": -1, "y": 33.50, "flipH": true}},
  "visualizations": [{"angle": 45, "size": 64, "z": 1e2}],
  "spritesheet": {"frames": {}, "meta": {"image": "chair.png", "size": {"w": 1, "h": 1}}},
  "extra": "a<b>&é"
}`

func TestRoundTrip(t *testing.T) {
	png := randomBytes(4096)
	conv, err := convertNitro(buildNitro(t,
		testFile{name: "chair.json", data: []byte(sampleJSON)},
		testFile{name: "chair.png", data: png},
	), "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if conv.Name != "chair" {
		t.Fatalf("bundle name %q, want chair", conv.Name)
	}
	b, err := readHab(conv.Out)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "chair" || len(b.Entries) != 2 {
		t.Fatalf("bundle %q with %d entries", b.Name, len(b.Entries))
	}
	if e := b.Entries[0]; e.Name != "chair.json" || e.MimeType != "application/json" || e.Offset != 0 {
		t.Fatalf("entry 0: %+v", e)
	}
	if e := b.Entries[1]; e.Name != "chair.png" || e.MimeType != "image/png" || e.Offset != b.Entries[0].StoredLength {
		t.Fatalf("entry 1: %+v", e)
	}
	if !bytes.Equal(b.Files["chair.png"], png) {
		t.Fatal("png bytes changed")
	}
	if !json.Valid(b.Files["chair.json"]) {
		t.Fatal("json entry is not valid JSON")
	}

	// The index is the official writer's compact JSON with its key order.
	h := b.Header
	idx, _, err := inflate(conv.Out[habHeaderSize:habHeaderSize+h.IndexStoredLen], int64(h.IndexOriginalLen))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"format":"hab","version":1,"name":"chair","entries":[` +
		`{"name":"chair.json","mimeType":"application/json","offset":0,"storedLength":` + itoa(b.Entries[0].StoredLength) +
		`,"originalLength":` + itoa(b.Entries[0].OriginalLength) + `,"compression":"deflate"},` +
		`{"name":"chair.png","mimeType":"image/png","offset":` + itoa(b.Entries[0].StoredLength) +
		`,"storedLength":4096,"originalLength":4096,"compression":"none"}]}`
	if string(idx) != want {
		t.Fatalf("index\n got %s\nwant %s", idx, want)
	}
	if len(conv.Out) != habHeaderSize+int(h.IndexStoredLen)+int(h.PayloadLen) {
		t.Fatal("total length mismatch")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestTransformAssetJSON(t *testing.T) {
	tests := []struct {
		name, in, fallback, want string
	}{
		{
			name: "inserted before spritesheet, order and value bytes kept",
			in:   sampleJSON,
			want: `{"name":"chair","logicType":"furniture_basic","assets":{"chair_64_a_0_0":{"x":-1,"y":33.50,"flipH":true}},` +
				`"visualizations":[{"angle":45,"size":64,"z":1e2}],"documentClass":"chair",` +
				`"spritesheet":{"frames":{},"meta":{"image":"chair.png","size":{"w":1,"h":1}}},"extra":"a<b>&é"}`,
		},
		{
			name:     "appended without spritesheet, fallback name",
			in:       "\xef\xbb\xbf{ \"type\" : \"fx\", \"k\\u0041\": [1, 2] }",
			fallback: "fx_1",
			want:     "{\"type\":\"fx\",\"k\\u0041\":[1,2],\"documentClass\":\"fx_1\"}",
		},
		{
			name: "existing documentClass kept",
			in:   `{"name":"a","documentClass":"Custom","spritesheet":{}}`,
			want: `{"name":"a","documentClass":"Custom","spritesheet":{}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := transformAssetJSON([]byte(tc.in), tc.fallback)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("\n got %s\nwant %s", got, tc.want)
			}
		})
	}
	for _, bad := range []string{`[1]`, `{"a":1`, `{"a":1} x`, `{"a":}`} {
		if _, _, err := transformAssetJSON([]byte(bad), "x"); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestDeflateOnlyIfSmaller(t *testing.T) {
	random := randomBytes(2048)
	repetitive := []byte(strings.Repeat("habbo ", 500))
	hab, err := writeHab("lib", []fileEntry{{Name: "a.png", Data: random}, {Name: "b.bin", Data: repetitive}, {Name: "c.txt", Data: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := readHab(hab)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		compression, mime string
	}{{"none", "image/png"}, {"deflate", "application/octet-stream"}, {"none", "text/plain"}}
	for i, e := range b.Entries {
		if e.Compression != want[i].compression || e.MimeType != want[i].mime {
			t.Errorf("%s: %s %s, want %+v", e.Name, e.Compression, e.MimeType, want[i])
		}
		if e.Compression == "deflate" && e.StoredLength >= e.OriginalLength {
			t.Errorf("%s: deflated but not smaller", e.Name)
		}
	}
	if !bytes.Equal(b.Files["a.png"], random) || !bytes.Equal(b.Files["b.bin"], repetitive) {
		t.Fatal("entry bytes changed")
	}
	if _, err := writeHab("lib", []fileEntry{{Name: "a"}, {Name: "a"}}); err == nil {
		t.Fatal("duplicate names accepted")
	}
}

func TestGzipWrappedNitro(t *testing.T) {
	data := []byte(`{"type":"effect"}`)
	files, err := readNitro(buildNitro(t, testFile{name: "fx.json", data: data, gzip: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Compression != "gzip" || !bytes.Equal(files[0].Data, data) {
		t.Fatalf("got %+v", files)
	}
	conv, err := convertNitro(buildNitro(t, testFile{name: "fx.json", data: data, gzip: true}), "fx")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := readHab(conv.Out)
	if got := string(b.Files["fx.json"]); got != `{"type":"effect","documentClass":"fx"}` || b.Name != "fx" {
		t.Fatalf("name %q json %s", b.Name, got)
	}
}

func TestTruncatedAndGarbageInput(t *testing.T) {
	full := buildNitro(t,
		testFile{name: "a.json", data: []byte(`{"name":"a"}`)},
		testFile{name: "a.png", data: randomBytes(300)},
	)
	for n := 0; n < len(full); n++ {
		if _, err := readNitro(full[:n]); err == nil {
			t.Fatalf("truncated to %d bytes: no error", n)
		}
	}
	if _, err := readNitro(append(bytes.Clone(full), 0)); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing byte: %v", err)
	}
	garbage := []byte{0, 1, 0, 1, 'x', 0, 0, 0, 3, 1, 2, 3}
	if _, err := readNitro(garbage); err == nil || !strings.Contains(err.Error(), "zlib") {
		t.Fatalf("garbage entry: %v", err)
	}
	if _, err := readNitro([]byte("PK\x03\x04 not a nitro file")); err == nil {
		t.Fatal("zip accepted")
	}
}

func TestReadHabRejectsCorruption(t *testing.T) {
	hab, err := writeHab("lib", []fileEntry{{Name: "a.txt", Data: []byte(strings.Repeat("a", 100))}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"short":   hab[:10],
		"magic":   append([]byte("HAX\x00"), hab[4:]...),
		"length":  append(bytes.Clone(hab), 0),
		"version": func() []byte { b := bytes.Clone(hab); b[4] = 2; return b }(),
	}
	for name, b := range cases {
		if _, err := readHab(b); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func zlibAt(t *testing.T, data []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// storedBytes returns an entry's bytes exactly as stored in the payload.
func storedBytes(t *testing.T, hab []byte, name string) ([]byte, habEntry) {
	t.Helper()
	b, err := readHab(hab)
	if err != nil {
		t.Fatal(err)
	}
	payload := hab[habHeaderSize+int(b.Header.IndexStoredLen):]
	for _, e := range b.Entries {
		if e.Name == name {
			return payload[e.Offset : e.Offset+e.StoredLength], e
		}
	}
	t.Fatalf("no entry %q", name)
	return nil, habEntry{}
}

func TestReuseNitroZlibStream(t *testing.T) {
	data := []byte(strings.Repeat("pixels and more pixels ", 400))
	weak := zlibAt(t, data, zlib.HuffmanOnly)
	if fresh, _ := deflate(data); len(fresh) >= len(weak) {
		t.Fatalf("fixture: fresh deflate %d not smaller than weak stream %d", len(fresh), len(weak))
	}
	noise := randomBytes(1000)
	jsonSrc := []byte(`{"name":"x"}`)
	jsonStream := zlibAt(t, jsonSrc, zlib.BestCompression)
	conv, err := convertNitro(buildNitro(t,
		testFile{name: "x.json", data: jsonSrc, stored: jsonStream},
		testFile{name: "x.png", data: data, stored: weak},
		testFile{name: "y.png", data: data, gzip: true},
		testFile{name: "z.png", data: data, stored: append(bytes.Clone(weak), 0)},
		testFile{name: "x.bin", data: data, stored: weak},
		testFile{name: "noise.png", data: noise},
	), "x")
	if err != nil {
		t.Fatal(err)
	}

	// A zlib-wrapped PNG is stored as the original stream, verbatim.
	if got, e := storedBytes(t, conv.Out, "x.png"); !bytes.Equal(got, weak) || e.Compression != "deflate" {
		t.Errorf("x.png: stored %d bytes (%s), want the original %d-byte stream", len(got), e.Compression, len(weak))
	}
	// Gzip-wrapped and trailing-junk streams are not reusable: fresh deflate.
	for _, name := range []string{"y.png", "z.png"} {
		if got, _ := storedBytes(t, conv.Out, name); bytes.Equal(got, weak) || len(got) >= len(weak) {
			t.Errorf("%s: stored %d bytes, want a fresh deflate", name, len(got))
		}
	}
	// Non-PNG entries keep the smaller of fresh and original.
	if got, _ := storedBytes(t, conv.Out, "x.bin"); len(got) >= len(weak) {
		t.Errorf("x.bin: stored %d bytes, want the smaller fresh deflate", len(got))
	}
	// The JSON entry changed (documentClass), so its stream is not reused.
	if got, _ := storedBytes(t, conv.Out, "x.json"); bytes.Equal(got, jsonStream) || string(conv.Entries[0].Data) != `{"name":"x","documentClass":"x"}` {
		t.Errorf("x.json: %s stored as %q", conv.Entries[0].Data, got)
	}
	// A reused stream larger than the raw bytes is not used: stored raw.
	if got, e := storedBytes(t, conv.Out, "noise.png"); e.Compression != "none" || !bytes.Equal(got, noise) {
		t.Errorf("noise.png: %s", e.Compression)
	}
}

func TestVerifyCatchesBadReusedStream(t *testing.T) {
	data := []byte(strings.Repeat("abc", 1000))
	files := []fileEntry{{Name: "a.bin", Data: data, Deflated: zlibAt(t, []byte("abc"), zlib.BestCompression)}}
	hab, err := writeHab("lib", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyHab(hab, "lib", files); err == nil {
		t.Fatal("verify accepted a stream that inflates to the wrong length")
	}
}
