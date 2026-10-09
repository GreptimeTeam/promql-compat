package target

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The harness executes what Fetch installs: a tarball that does not match
// the published checksum must never end up in the cache.
func TestFetchVerifiesChecksum(t *testing.T) {
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	payload := []byte("#!/bin/sh\n")
	if err := tw.WriteHeader(&tar.Header{Name: "victoria-metrics-prod", Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(payload)
	tw.Close()
	gz.Close()

	tarball := fmt.Sprintf("victoria-metrics-%s-%s-v1.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(tgz.Bytes())
	published := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case tarball:
			w.Write(tgz.Bytes())
		case fmt.Sprintf("victoria-metrics-%s-%s-v1_checksums.txt", runtime.GOOS, runtime.GOARCH):
			fmt.Fprintf(w, "%s  %s\n", published, tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	releaseBase = srv.URL
	root := t.TempDir()
	cacheRoot = func() (string, error) { return root, nil }

	good := published
	published = "00" + good[2:]
	if _, err := Fetch(context.Background(), "victoriametrics", "v1"); err == nil {
		t.Fatal("tampered tarball accepted")
	}
	if bins, _ := filepath.Glob(filepath.Join(root, "*", "victoria-metrics-prod")); len(bins) != 0 {
		t.Fatalf("binary installed despite checksum mismatch: %v", bins)
	}

	published = good
	bin, err := Fetch(context.Background(), "victoriametrics", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(bin); !bytes.Equal(got, payload) {
		t.Fatalf("installed %q, want %q", got, payload)
	}
}
