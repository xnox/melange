// simdeps rewrites APKINDEXes to simulate the versioned "depends:" that
// melange's SCA would generate if every package were rebuilt.
//
// For the newest build of every package name in each repository, it
// reads the build environment recorded in the APK's .melange.yaml
// (fetching only the APK's control section), and for each "so:X" and
// "pc:X" depend already in the index adds "so-ver:X>=V" / "pc:X>=V",
// where V is the version of the provider pinned in that build
// environment.  This mirrors determineProviderVersion in
// melange/pkg/sca/sca.go.  Older builds are left as they are.
//
// Usage:
//
//	simdeps -arch x86_64 -out DIR [-cache DIR] [-no-versioned-provides GLOBS] REPO...
//
// -no-versioned-provides takes a comma separated list of origin globs
// (e.g. "hwloc-cuda-*,zlib-ng,cyrus-sasl*") whose packages are treated
// as not publishing "so-ver:" provides, simulating a provider side opt
// out: their consumers keep unversioned "so:" depends.
//
// writes DIR/<repo host+path>/<arch>/APKINDEX.tar.gz (unsigned) for
// each REPO, and DIR/<arch>-report.tsv listing every change.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"chainguard.dev/apko/pkg/apk/apk"
	"chainguard.dev/apko/pkg/apk/auth"
	"gopkg.in/yaml.v3"
)

// section is the part of a (sub)package in .melange.yaml we need.
type section struct {
	Name    string `yaml:"name" json:"name"`
	Options *struct {
		NoVersionedShlibDeps bool `yaml:"no-versioned-shlib-deps" json:"no_versioned"`
	} `yaml:"options" json:"options,omitempty"`
	Dependencies struct {
		Runtime []string `yaml:"runtime" json:"runtime,omitempty"`
	} `yaml:"dependencies" json:"dependencies"`
}

// recorded is what we keep from an APK's .melange.yaml.
type recorded struct {
	Missing     bool      `json:"missing,omitempty"` // no .melange.yaml
	Package     section   `yaml:"package" json:"package"`
	Subpackages []section `yaml:"subpackages" json:"subpackages,omitempty"`
	Environment struct {
		Contents struct {
			Packages []string `yaml:"packages" json:"packages"`
		} `yaml:"contents" json:"contents"`
	} `yaml:"environment" json:"environment"`
}

func (r *recorded) section(name string) *section {
	if r.Package.Name == name {
		return &r.Package
	}
	for i := range r.Subpackages {
		if r.Subpackages[i].Name == name {
			return &r.Subpackages[i]
		}
	}
	return nil
}

func (r *recorded) pins() map[string]string {
	m := make(map[string]string, len(r.Environment.Contents.Packages))
	for _, p := range r.Environment.Contents.Packages {
		if name, ver, ok := strings.Cut(p, "="); ok {
			m[name] = ver
		}
	}
	return m
}

// fetchRecorded returns the .melange.yaml recorded in the APK at u,
// fetching as little of the APK as it can.
func fetchRecorded(ctx context.Context, u string) (*recorded, error) {
	for n := int64(64 << 10); ; n *= 4 {
		b, whole, err := fetchRange(ctx, u, n)
		if err != nil {
			return nil, err
		}
		rec, err := parseControl(b)
		if err == nil {
			return rec, nil
		}
		if whole || !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
	}
}

func fetchRange(ctx context.Context, u string, n int64) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	if err := auth.DefaultAuthenticators.AddAuth(ctx, req); err != nil {
		return nil, false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	default:
		return nil, false, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, n))
	if err != nil {
		return nil, false, err
	}
	whole := int64(len(b)) < n
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		var start, end, total int64
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); err == nil {
			whole = end+1 >= total
		}
	}
	return b, whole, nil
}

// parseControl finds .melange.yaml in the control section of the
// (possibly truncated) APK b.  An APK is an optional signature stream,
// a control stream and a data stream, each gzipped separately.
func parseControl(b []byte) (*recorded, error) {
	br := bytes.NewReader(b)
	gz, err := gzip.NewReader(br)
	if err != nil {
		return nil, err
	}
	for stream := 0; stream < 2; stream++ {
		if stream > 0 {
			if err := gz.Reset(br); err != nil {
				return nil, err
			}
		}
		gz.Multistream(false)
		tr := tar.NewReader(gz)
		signature := false
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if stream == 0 && strings.HasPrefix(hdr.Name, ".SIGN.") {
				signature = true
				break
			}
			if hdr.Name == ".melange.yaml" {
				// Read it whole first, so that a truncated fetch
				// reports io.ErrUnexpectedEOF and we fetch more.
				y, err := io.ReadAll(tr)
				if err != nil {
					return nil, err
				}
				var rec recorded
				if err := yaml.Unmarshal(y, &rec); err != nil {
					return nil, fmt.Errorf("decoding .melange.yaml: %w", err)
				}
				return &rec, nil
			}
		}
		if !signature {
			break
		}
		// Skip the rest of the signature stream.
		if _, err := io.Copy(io.Discard, gz); err != nil {
			return nil, err
		}
	}
	return &recorded{Missing: true}, nil
}

type cache struct{ dir string }

func (c cache) get(ctx context.Context, u string) (*recorded, error) {
	key := fmt.Sprintf("%x.json", sha256.Sum256([]byte(u)))
	p := filepath.Join(c.dir, key)
	if b, err := os.ReadFile(p); err == nil {
		var rec recorded
		if err := json.Unmarshal(b, &rec); err == nil {
			return &rec, nil
		}
	}
	rec, err := fetchRecorded(ctx, u)
	if err != nil {
		return nil, err
	}
	if b, err := json.Marshal(rec); err == nil {
		_ = os.WriteFile(p, b, 0o644)
	}
	return rec, nil
}

// universe is every package in every index, by what it provides.
type universe map[string][]*apk.Package

func newUniverse(indexes []*apk.APKIndex) universe {
	u := universe{}
	for _, idx := range indexes {
		for _, p := range idx.Packages {
			for _, prov := range p.Provides {
				name, _, _ := strings.Cut(prov, "=")
				u[name] = append(u[name], p)
			}
		}
	}
	for _, ps := range u {
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].BuildTime.After(ps[j].BuildTime) })
	}
	return u
}

// noVersionedProvides are the origin globs treated as publishing no
// "so-ver:" provides.
var noVersionedProvides []string

func optedOut(origin string) bool {
	return slices.ContainsFunc(noVersionedProvides, func(glob string) bool {
		ok, _ := path.Match(glob, origin)
		return ok
	})
}

// providerVersion mirrors determineProviderVersion in
// melange/pkg/sca/sca.go: the version of the package pinned in the
// build environment that provides resolve, if it publishes a versioned
// "provides:" for versioned.
func (u universe) providerVersion(e *apk.Package, pins map[string]string, resolve, versioned string) (string, error) {
	candidates := u[resolve]
	if len(candidates) == 0 {
		// "nothing provides": assume one of the packages being built
		// provides it.
		return e.Version, nil
	}
	for _, c := range candidates {
		if strings.HasPrefix(versioned, "so-ver:") && optedOut(c.Origin) {
			// Publishes no "so-ver:" provides, not even to its
			// own subpackages.
			if c.Origin == e.Origin {
				return "", nil
			}
			if _, ok := pins[c.Name]; ok {
				return "", nil
			}
			continue
		}
		if c.Origin == e.Origin {
			// One of the packages being built (@CURRENT@).
			return e.Version, nil
		}
		installed, ok := pins[c.Name]
		if !ok {
			continue
		}
		cv, err := apk.ParseVersion(c.Version)
		if err != nil {
			return "", err
		}
		iv, err := apk.ParseVersion(installed)
		if err != nil {
			return "", err
		}
		if apk.CompareVersions(cv, iv) < 0 {
			return "", fmt.Errorf("could not find suitable package providing %s>=%s (found %s=%s instead)", versioned, installed, versioned, c.Version)
		}
		if slices.ContainsFunc(c.Provides, func(p string) bool {
			name, ver, ok := strings.Cut(p, "=")
			if !ok || name != versioned {
				return false
			}
			_, err := apk.ParseVersion(ver)
			return err == nil
		}) {
			return installed, nil
		}
		return "", nil
	}
	return "", nil
}

type change struct {
	repo, pkg, version, note string
}

func main() {
	arch := flag.String("arch", "x86_64", "architecture")
	out := flag.String("out", "", "output directory")
	cacheDir := flag.String("cache", "", "cache directory for fetched .melange.yaml (default OUT/cache)")
	jobs := flag.Int("j", 32, "concurrent fetches")
	optOut := flag.String("no-versioned-provides", "", "comma separated origin globs treated as publishing no so-ver: provides")
	flag.Parse()
	if *optOut != "" {
		noVersionedProvides = strings.Split(*optOut, ",")
	}
	if *out == "" || flag.NArg() == 0 {
		log.Fatal("usage: simdeps -arch ARCH -out DIR REPO...")
	}
	if *cacheDir == "" {
		*cacheDir = filepath.Join(*out, "cache")
	}
	if err := os.MkdirAll(*cacheDir, 0o755); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	repos := flag.Args()

	indexes := make([]*apk.APKIndex, len(repos))
	for i, repo := range repos {
		named, err := apk.GetRepositoryIndexes(ctx, []string{repo}, nil, *arch,
			apk.WithIgnoreSignatures(true), apk.WithHTTPClient(http.DefaultClient))
		if err != nil {
			log.Fatalf("%s: %v", repo, err)
		}
		if len(named) != 1 {
			log.Fatalf("%s: got %d indexes", repo, len(named))
		}
		idx := &apk.APKIndex{}
		for _, rp := range named[0].Packages() {
			idx.Packages = append(idx.Packages, rp.Package)
		}
		indexes[i] = idx
		log.Printf("%s: %d packages", repo, len(idx.Packages))
	}
	u := newUniverse(indexes)

	// The newest build of each package name, per repository.
	type target struct {
		repo int
		pkg  *apk.Package
		url  string
	}
	var targets []target
	for i, idx := range indexes {
		latest := map[string]*apk.Package{}
		for _, p := range idx.Packages {
			if q, ok := latest[p.Name]; !ok || p.BuildTime.After(q.BuildTime) {
				latest[p.Name] = p
			}
		}
		for _, p := range latest {
			targets = append(targets, target{i, p, fmt.Sprintf("%s/%s/%s-%s.apk", strings.TrimSuffix(repos[i], "/"), *arch, p.Name, p.Version)})
		}
	}
	log.Printf("%d packages to rewrite", len(targets))

	// Every APK a build emits records the same .melange.yaml, so fetch
	// it once per build, preferring the origin package's own APK.
	type build struct {
		repo            int
		origin, version string
	}
	builds := map[build]string{}
	for _, t := range targets {
		b := build{t.repo, t.pkg.Origin, t.pkg.Version}
		if _, ok := builds[b]; !ok || t.pkg.Name == t.pkg.Origin {
			builds[b] = t.url
		}
	}
	log.Printf("%d builds to fetch", len(builds))

	c := cache{*cacheDir}
	type result struct {
		rec *recorded
		err error
	}
	var mu sync.Mutex
	results := map[build]result{}
	var done atomic.Int64
	var wg sync.WaitGroup
	sem := make(chan struct{}, *jobs)
	for b, url := range builds {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rec, err := c.get(ctx, url)
			mu.Lock()
			results[b] = result{rec, err}
			mu.Unlock()
			if n := done.Add(1); n%1000 == 0 {
				log.Printf("fetched %d/%d", n, len(builds))
			}
		}()
	}
	wg.Wait()

	recs := make([]*recorded, len(targets))
	errs := make([]error, len(targets))
	for i, t := range targets {
		r := results[build{t.repo, t.pkg.Origin, t.pkg.Version}]
		recs[i], errs[i] = r.rec, r.err
	}

	var changes []change
	var stats struct{ rewritten, added, missing, fetchErr, optOut, resolveErr int }
	for i, t := range targets {
		repo := repos[t.repo]
		e := t.pkg
		if errs[i] != nil {
			stats.fetchErr++
			changes = append(changes, change{repo, e.Name, e.Version, "FETCH-ERROR " + errs[i].Error()})
			continue
		}
		rec := recs[i]
		if rec.Missing {
			stats.missing++
			continue
		}
		sec := rec.section(e.Name)
		if sec != nil && sec.Options != nil && sec.Options.NoVersionedShlibDeps {
			stats.optOut++
			continue
		}
		var explicit []string
		if sec != nil {
			explicit = sec.Dependencies.Runtime
		}
		pins := rec.pins()

		var added []string
		for _, d := range e.Dependencies {
			if strings.ContainsAny(d, "<>=~") || strings.HasPrefix(d, "!") || slices.Contains(explicit, d) {
				continue
			}
			var versioned string
			switch {
			case strings.HasPrefix(d, "so:"):
				versioned = "so-ver:" + strings.TrimPrefix(d, "so:")
			case strings.HasPrefix(d, "pc:"):
				versioned = d
			default:
				continue
			}
			if slices.ContainsFunc(e.Dependencies, func(x string) bool {
				return strings.HasPrefix(x, versioned+">") || strings.HasPrefix(x, versioned+"=")
			}) {
				continue
			}
			v, err := u.providerVersion(e, pins, d, versioned)
			if err != nil {
				stats.resolveErr++
				changes = append(changes, change{repo, e.Name, e.Version, "RESOLVE-ERROR " + err.Error()})
				continue
			}
			if v != "" {
				added = append(added, versioned+">="+v)
			}
		}
		if len(added) > 0 {
			stats.rewritten++
			stats.added += len(added)
			e.Dependencies = append(e.Dependencies, added...)
			for _, a := range added {
				changes = append(changes, change{repo, e.Name, e.Version, a})
			}
		}
	}

	for i, idx := range indexes {
		pu, err := url.Parse(repos[i])
		if err != nil {
			log.Fatal(err)
		}
		dir := filepath.Join(*out, pu.Host+pu.Path, *arch)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
		archive, err := apk.ArchiveFromIndex(idx)
		if err != nil {
			log.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, "APKINDEX.tar.gz"))
		if err != nil {
			log.Fatal(err)
		}
		if _, err := io.Copy(f, archive); err != nil {
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote %s", f.Name())
	}

	var rep bytes.Buffer
	for _, ch := range changes {
		fmt.Fprintf(&rep, "%s\t%s\t%s\t%s\n", ch.repo, ch.pkg, ch.version, ch.note)
	}
	rp := filepath.Join(*out, *arch+"-report.tsv")
	if err := os.WriteFile(rp, rep.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("%d packages rewritten, %d versioned depends added; %d without .melange.yaml, %d opted out, %d fetch errors, %d resolve errors; report in %s",
		stats.rewritten, stats.added, stats.missing, stats.optOut, stats.fetchErr, stats.resolveErr, rp)
}
