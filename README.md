# hab-converter

Convert Nitro `.nitro` asset bundles into Habbo Classic `.hab` bundles for the official v75 client, and convert compatible JSON-based `.hab` bundles back into `.nitro`.

The converter handles individual files or whole directories of furniture, clothing, effects and pet assets. It runs as a standalone command-line program on Linux, Windows and macOS.

## Download

Get the binary for your system from [Releases](https://github.com/plus-emulator/hab-converter/releases/latest):

- Linux: `hab-converter-linux-amd64` or `hab-converter-linux-arm64`
- Windows: `hab-converter-windows-amd64.exe`
- macOS: `hab-converter-darwin-amd64` for Intel, or `hab-converter-darwin-arm64` for Apple silicon

Each release includes `SHA256SUMS` for verifying the downloads. Prebuilt binaries do not require Go.

On Linux or macOS, make the downloaded file executable and rename it to `hab-converter`. For example, on Linux x86-64:

```bash
chmod +x hab-converter-linux-amd64
mv hab-converter-linux-amd64 hab-converter
./hab-converter -version
```

On Windows, rename the binary to `hab-converter.exe` and run it from PowerShell:

```powershell
.\hab-converter.exe -version
```

The examples below use `./hab-converter`; on Windows, use `.\hab-converter.exe` instead.

## Usage

Put flags **before** the input paths. Quote paths containing spaces.

### Nitro to HAB

Convert a single file. The default direction is `-to hab`:

```bash
./hab-converter furniture/throne.nitro
```

This writes `furniture/throne.hab` next to the input.

Convert a directory recursively into a separate output directory:

```bash
./hab-converter -o hab-assets/ nitro-assets/
```

For example, `nitro-assets/furniture/throne.nitro` becomes `hab-assets/furniture/throne.hab`.

You can pass multiple files or directories. With `-o`, each directory's contents are placed relative to that output root. Inputs that would produce the same output path are reported as conflicts.

### HAB to Nitro

```bash
./hab-converter -to nitro -o nitro-assets/ hab-assets/
```

Only **JSON asset bundles** can be converted to Nitro: at least one `.json` entry, with any remaining entries being PNG, GIF or JPEG images. Official XML-based HAB libraries and bundles with unsupported metadata or entry types are rejected. This is not an XML-to-JSON asset converter.

### Inspect a bundle

Print the header, metadata and entry table without converting files:

```bash
./hab-converter -inspect furniture/throne.hab
./hab-converter -inspect hab-assets/ nitro-assets/
```

Inspection searches directories recursively for both `.hab` and `.nitro` files and reports entry names, MIME types, original and stored sizes, and compression.

### Options

- `-to hab` or `-to nitro`: output format; defaults to `hab`.
- `-o <directory>`: output root; defaults to writing beside each input.
- `-j <number>`: parallel workers; defaults to the number of logical CPUs. Must be at least 1.
- `-force`: convert even if the existing output is newer than the input.
- `-q`: hide individual success and skip messages. Errors and the final conversion summary are still printed.
- `-inspect`: inspect bundles instead of converting them.
- `-version`: print the version.
- `-h`: show help and examples.

Input extension matching is case-insensitive. Existing outputs newer than their inputs are skipped unless `-force` is supplied; other existing outputs may be replaced. Source files are not modified.

Exit codes are `0` for success, `1` if any conversion or inspection fails, and `2` for invalid command-line arguments.

## Conversion details

When converting to HAB, JSON entries missing `documentClass` receive one derived from the asset name, inserted before `spritesheet` when present. An existing `documentClass` is preserved. Other entries retain their decoded contents. The resulting bundle is decoded again and checked against the intended entries before it is written.

When converting back to Nitro, a `documentClass` matching the bundle name or the JSON asset's own name is removed. Every entry is zlib-compressed, and the generated bundle is decoded and verified before writing. Existing compatible compressed streams are reused where possible; conversions are not guaranteed to reproduce the original container bytes.

## Build from source

Use Go 1.27 or newer, as specified in [`go.mod`](go.mod). The converter uses only the Go standard library.

```bash
git clone https://github.com/plus-emulator/hab-converter.git
cd hab-converter
go build -o hab-converter .
./hab-converter -h
```

On Windows, build with `go build -o hab-converter.exe .`.

Run the existing checks:

```bash
go vet ./...
go test ./...
```

The [release workflow](.github/workflows/release.yml) builds binaries for the platforms listed above when a `v*` tag is pushed and embeds the tag as the version. Local builds report `dev` unless a version is supplied with `-ldflags "-X main.version=<version>"`.

## Licence

Licensed under the GNU General Public License, version 3 only (`GPL-3.0-only`). See [LICENSE](LICENSE).
