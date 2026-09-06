package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedBinaryCannotSelfUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "om")
	if err := CheckManaged(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManagedMarker), []byte("0.1.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckManaged(path); err == nil {
		t.Fatal("managed binary allowed to self-update")
	}
}
func TestChecksumSelectsExactAsset(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	manifest := []byte(digest + "  exact.tar.gz\n" + strings.Repeat("cd", 32) + "  other.tar.gz\n")
	got, err := checksum(manifest, "exact.tar.gz")
	if err != nil || got != digest {
		t.Fatal(got, err)
	}
	if _, err = checksum(manifest, "missing.tar.gz"); err == nil {
		t.Fatal("accepted missing checksum")
	}
	if _, err = checksum(append(manifest, manifest...), "exact.tar.gz"); err == nil {
		t.Fatal("accepted ambiguous checksum")
	}
}
func archive(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writer := tar.NewWriter(gz)
	for _, name := range names {
		body := "synthetic binary"
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0700, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func TestArchiveExtraction(t *testing.T) {
	for _, names := range [][]string{{"../escape"}, {"om", "om"}, {"LICENSE"}} {
		if err := extract(bytes.NewReader(archive(t, names...)), filepath.Join(t.TempDir(), "candidate")); err == nil {
			t.Fatalf("accepted %v", names)
		}
	}
	path := filepath.Join(t.TempDir(), "candidate")
	if err := extract(bytes.NewReader(archive(t, "om", "LICENSE")), path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "synthetic binary" {
		t.Fatal(string(data), err)
	}
}
