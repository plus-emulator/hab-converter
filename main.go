// Command hab-converter converts between Nitro .nitro asset bundles and
// Habbo Classic .hab asset bundles readable by the official v75 client.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `hab-converter converts Nitro .nitro asset bundles (furniture, clothes,
effects, pets) into Habbo Classic .hab asset bundles readable by the official
v75 client, and JSON asset .hab bundles back into .nitro.

Usage:
  hab-converter [flags] <input>...
  hab-converter -to nitro [flags] <input>...
  hab-converter -inspect <file.hab|file.nitro|dir>...

Inputs are files or directories, searched recursively (case-insensitive) for
*.nitro with -to hab and *.hab with -to nitro. Without -o each output is
written next to its input; with -o, files keep their path relative to the
input directory they came from.

-to hab: each JSON entry gets "documentClass" (its "name") inserted before
"spritesheet", like the official files; other entries are copied unchanged.
Every written file is re-read and validated with the official reader rules.

-to nitro: only JSON asset bundles convert (JSON entries plus png/gif/jpeg
images); official XML libraries are rejected. A "documentClass" equal to the
bundle name is removed again, and every entry is stored zlib-compressed as
the Nitro renderer expects. Every written file is re-read and checked.

Outputs newer than their input are skipped unless -force is given.

Flags:
`

const examples = `
Examples:
  hab-converter furniture/throne.nitro
  hab-converter -o hab/ -q nitro-assets/furniture/nitro nitro-assets/clothes/nitro
  hab-converter -to nitro -o nitro/ hab/
  hab-converter -inspect hab/throne.hab
`

// direction is one conversion direction, selected with -to.
type direction struct {
	from, to string // file extensions
	convert  func(src []byte, baseName string) (*conversion, error)
}

var directions = map[string]direction{
	"hab":   {from: ".nitro", to: ".hab", convert: convertNitro},
	"nitro": {from: ".hab", to: ".nitro", convert: convertHab},
}

type options struct {
	dir     direction
	outDir  string
	workers int
	force   bool
	quiet   bool
}

type job struct {
	in, out, baseName string
}

type result struct {
	job      job
	skipped  bool
	err      error
	entries  int
	inBytes  int64
	outBytes int64
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("hab-converter", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var opts options
	var inspect, showVersion bool
	var to string
	fl.StringVar(&to, "to", "hab", "output format: `hab` (from .nitro) or nitro (from .hab)")
	fl.StringVar(&opts.outDir, "o", "", "output root `dir` (default: next to each input)")
	fl.IntVar(&opts.workers, "j", runtime.NumCPU(), "number of parallel workers")
	fl.BoolVar(&opts.force, "force", false, "convert even when the output is newer than the input")
	fl.BoolVar(&opts.quiet, "q", false, "do not print per-file success and skip lines")
	fl.BoolVar(&inspect, "inspect", false, "print header and entry table of .hab/.nitro files instead of converting")
	fl.BoolVar(&showVersion, "version", false, "print the version and exit")
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), usage)
		fl.PrintDefaults()
		fmt.Fprint(fl.Output(), examples)
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if showVersion {
		fmt.Fprintf(stdout, "hab-converter %s\n", version)
		return 0
	}
	dir, ok := directions[to]
	if !ok {
		fmt.Fprintf(stderr, "hab-converter: -to must be hab or nitro, got %q\n", to)
		return 2
	}
	opts.dir = dir
	if fl.NArg() == 0 {
		fl.Usage()
		return 2
	}
	if opts.workers < 1 {
		fmt.Fprintln(stderr, "hab-converter: -j must be at least 1")
		return 2
	}
	if inspect {
		return runInspect(fl.Args(), stdout, stderr)
	}
	return runConvert(fl.Args(), opts, stdout, stderr)
}

func runConvert(inputs []string, opts options, stdout, stderr io.Writer) int {
	start := time.Now()
	jobs, planErrs := planJobs(inputs, opts.outDir, opts.dir)
	for _, err := range planErrs {
		fmt.Fprintf(stderr, "FAIL %v\n", err)
	}

	jobCh := make(chan job)
	resCh := make(chan result)
	var wg sync.WaitGroup
	for range min(opts.workers, max(len(jobs), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				resCh <- convertOne(j, opts.dir, opts.force)
			}
		}()
	}
	go func() {
		for _, j := range jobs {
			jobCh <- j
		}
		close(jobCh)
		wg.Wait()
		close(resCh)
	}()

	var converted, skipped int
	failed := len(planErrs)
	var inBytes, outBytes int64
	for r := range resCh {
		switch {
		case r.err != nil:
			failed++
			fmt.Fprintf(stderr, "FAIL %s: %v\n", r.job.in, r.err)
		case r.skipped:
			skipped++
			if !opts.quiet {
				fmt.Fprintf(stdout, "skip %s (up to date)\n", r.job.out)
			}
		default:
			converted++
			inBytes += r.inBytes
			outBytes += r.outBytes
			if !opts.quiet {
				fmt.Fprintf(stdout, "ok   %s -> %s (%d entries, %d -> %d bytes)\n", r.job.in, r.job.out, r.entries, r.inBytes, r.outBytes)
			}
		}
	}
	fmt.Fprintf(stdout, "converted %d, skipped %d, failed %d; %s -> %s; %s\n",
		converted, skipped, failed, formatBytes(inBytes), formatBytes(outBytes),
		time.Since(start).Round(time.Millisecond))
	if failed > 0 {
		return 1
	}
	return 0
}

// planJobs expands inputs into conversion jobs, rejecting outputs that two
// inputs would both write.
func planJobs(inputs []string, outDir string, dir direction) ([]job, []error) {
	var jobs []job
	var errs []error
	owner := map[string]string{}
	add := func(in, out string) {
		if prev, ok := owner[out]; ok {
			errs = append(errs, fmt.Errorf("%s: output %s is also produced by %s", in, out, prev))
			return
		}
		if filepath.Clean(in) == filepath.Clean(out) {
			errs = append(errs, fmt.Errorf("%s: output would overwrite the input", in))
			return
		}
		owner[out] = in
		jobs = append(jobs, job{in: in, out: out, baseName: baseNameNoExt(in)})
	}
	for _, input := range inputs {
		info, err := os.Stat(input)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !info.IsDir() {
			if !strings.EqualFold(filepath.Ext(input), dir.from) {
				errs = append(errs, fmt.Errorf("%s: not a %s file (see -to)", input, dir.from))
				continue
			}
			root := filepath.Dir(input)
			if outDir != "" {
				root = outDir
			}
			add(input, filepath.Join(root, withExt(filepath.Base(input), dir.to)))
			continue
		}
		err = filepath.WalkDir(input, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				errs = append(errs, err)
				if d != nil && d.IsDir() && path != input {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() || !strings.EqualFold(filepath.Ext(path), dir.from) {
				return nil
			}
			out := withExt(path, dir.to)
			if outDir != "" {
				rel, err := filepath.Rel(input, path)
				if err != nil {
					errs = append(errs, err)
					return nil
				}
				out = filepath.Join(outDir, withExt(rel, dir.to))
			}
			add(path, out)
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return jobs, errs
}

func withExt(path, ext string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ext
}

func convertOne(j job, dir direction, force bool) result {
	r := result{job: j}
	inInfo, err := os.Stat(j.in)
	if err != nil {
		r.err = err
		return r
	}
	if !force {
		if outInfo, err := os.Stat(j.out); err == nil && outInfo.ModTime().After(inInfo.ModTime()) {
			r.skipped = true
			return r
		}
	}
	src, err := os.ReadFile(j.in)
	if err != nil {
		r.err = err
		return r
	}
	conv, err := dir.convert(src, j.baseName)
	if err != nil {
		r.err = err
		return r
	}
	if err := writeFileAtomic(j.out, conv.Out); err != nil {
		r.err = err
		return r
	}
	r.entries = len(conv.Entries)
	r.inBytes = int64(len(src))
	r.outBytes = int64(len(conv.Out))
	return r
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
