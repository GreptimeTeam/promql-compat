package target

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// release describes the GitHub release assets of a product. tarball and
// checksum take os, arch and tag in that order; so does entry if it has verbs.
type release struct {
	repo     string
	tarball  string
	checksum string // file with "<sha256>" or "<sha256>  <file>" lines
	entry    string // path of the binary inside the tarball
}

var releases = map[string]release{
	"greptime": {
		repo:     "GreptimeTeam/greptimedb",
		tarball:  "greptime-%s-%s-%s.tar.gz",
		checksum: "greptime-%s-%s-%s.sha256sum",
		entry:    "greptime-%s-%s-%s/greptime",
	},
	"victoriametrics": {
		repo:     "VictoriaMetrics/VictoriaMetrics",
		tarball:  "victoria-metrics-%s-%s-%s.tar.gz",
		checksum: "victoria-metrics-%s-%s-%s_checksums.txt",
		entry:    "victoria-metrics-prod",
	},
}

// Overridden in tests.
var (
	releaseBase = "https://github.com"
	cacheRoot   = func() (string, error) {
		d, err := os.UserCacheDir()
		return filepath.Join(d, "promql-compat"), err
	}
)

// Fetch returns the binary of product at release tag for this platform,
// downloading it into the user cache on first use. The tarball must match
// the published sha256; a cached binary was verified when it was installed.
func Fetch(ctx context.Context, product, tag string) (string, error) {
	rel, ok := releases[product]
	if !ok {
		return "", fmt.Errorf("unknown product %q", product)
	}
	f := func(format string) string { return fmt.Sprintf(format, runtime.GOOS, runtime.GOARCH, tag) }
	tarball, entry := f(rel.tarball), rel.entry
	if strings.Contains(entry, "%") {
		entry = f(entry)
	}
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, fmt.Sprintf("%s-%s-%s-%s", product, tag, runtime.GOOS, runtime.GOARCH))
	bin := filepath.Join(dir, path.Base(entry))
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := fmt.Sprintf("%s/%s/releases/download/%s/", releaseBase, rel.repo, tag)

	sums, err := get(ctx, base+f(rel.checksum))
	if err != nil {
		return "", err
	}
	want, err := checksumFor(string(sums), tarball)
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(dir, "download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := getTo(ctx, base+tarball, tmp); err != nil {
		return "", err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, tmp); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("%s: sha256 %s, published %s", tarball, got, want)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if err := extract(tmp, entry, bin); err != nil {
		return "", fmt.Errorf("%s: %w", tarball, err)
	}
	return bin, nil
}

// checksumFor finds the sha256 of file in a checksum listing: either a bare
// hash, or "<hash>  <file>" lines.
func checksumFor(listing, file string) (string, error) {
	for _, line := range strings.Split(listing, "\n") {
		fs := strings.Fields(line)
		switch {
		case len(fs) == 1:
			return strings.ToLower(fs[0]), nil
		case len(fs) == 2 && fs[1] == file:
			return strings.ToLower(fs[0]), nil
		}
	}
	return "", fmt.Errorf("no checksum for %s", file)
}

// extract writes entry of the gzipped tarball r to dst atomically.
func extract(r io.Reader, entry, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s not in archive", entry)
		}
		if err != nil {
			return err
		}
		if path.Clean(hdr.Name) != entry {
			continue
		}
		out, err := os.CreateTemp(filepath.Dir(dst), "bin-*")
		if err != nil {
			return err
		}
		defer os.Remove(out.Name())
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		if err := os.Chmod(out.Name(), 0o755); err != nil {
			return err
		}
		return os.Rename(out.Name(), dst)
	}
}

func get(ctx context.Context, url string) ([]byte, error) {
	var b bytes.Buffer
	err := getTo(ctx, url, &b)
	return b.Bytes(), err
}

func getTo(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}
