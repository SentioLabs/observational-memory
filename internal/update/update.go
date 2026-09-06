// Package update integrates explicit CLI updates. Hooks never invoke this package's network operations.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	selfupdate "github.com/sentiolabs/go-selfupdate"
)

const ManagedMarker = ".observational-memory-managed"
const Repository = "sentiolabs/observational-memory"

var validTag = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)

type channelStore struct{}

func configPath() (string, error) {
	root, err := os.UserConfigDir()
	return filepath.Join(root, "observational-memory", "updates.json"), err
}
func (channelStore) Channel() (selfupdate.Channel, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return selfupdate.ChannelStable, nil
	}
	if err != nil {
		return "", err
	}
	var config struct {
		Channel selfupdate.Channel `json:"channel"`
	}
	err = json.Unmarshal(data, &config)
	return config.Channel, err
}
func (channelStore) SetChannel(channel selfupdate.Channel) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".updates-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = json.NewEncoder(tmp).Encode(map[string]any{"channel": channel})
	if err = errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
func executable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}
func CheckManaged(path string) error {
	_, err := os.Stat(filepath.Join(filepath.Dir(path), ManagedMarker))
	if err == nil {
		return fmt.Errorf("this binary is managed by a marketplace plugin; use that plugin's setup to update its pinned runtime")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func New(version string) *selfupdate.Updater {
	return &selfupdate.Updater{
		Name: "observational-memory", Version: version,
		Source: &selfupdate.GitHubSource{Owner: "sentiolabs", Repo: "observational-memory"},
		Store:  channelStore{}, Installer: Installer{},
		PreInstall: func(_ context.Context, _, _ string) error {
			path, err := executable()
			if err != nil {
				return err
			}
			return CheckManaged(path)
		},
	}
}

type Installer struct{}

func (Installer) Install(ctx context.Context, tag string) error {
	path, err := executable()
	if err != nil {
		return err
	}
	if err = CheckManaged(path); err != nil {
		return err
	}
	if !validTag.MatchString(tag) {
		return fmt.Errorf("invalid release tag %q", tag)
	}
	version := strings.TrimPrefix(tag, "v")
	asset := fmt.Sprintf("observational-memory_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	base := "https://github.com/" + Repository + "/releases/download/v" + version + "/"
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("update redirect requires HTTPS")
		}
		return nil
	}}
	dir, err := os.MkdirTemp(filepath.Dir(path), ".om-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	checksums, err := os.Create(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		return err
	}
	err = download(ctx, client, base+"checksums.txt", checksums, 1<<20)
	if err = errors.Join(err, checksums.Close()); err != nil {
		return err
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		return err
	}
	digest, err := checksum(manifest, asset)
	if err != nil {
		return err
	}
	archive, err := os.Create(filepath.Join(dir, asset))
	if err != nil {
		return err
	}
	defer archive.Close()
	hash := sha256.New()
	if err = download(ctx, client, base+asset, io.MultiWriter(archive, hash), 64<<20); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("release checksum mismatch")
	}
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	candidate := filepath.Join(dir, "observational-memory")
	if err = extract(archive, candidate); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, candidate, "--version").Output()
	if err != nil {
		return fmt.Errorf("validate downloaded binary: %w", err)
	}
	if strings.TrimSpace(string(output)) != "observational-memory "+version {
		return fmt.Errorf("downloaded binary version mismatch")
	}
	return os.Rename(candidate, path)
}
func download(ctx context.Context, client *http.Client, address string, dest io.Writer, limit int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", response.StatusCode)
	}
	count, err := io.Copy(dest, io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if count > limit {
		return fmt.Errorf("download exceeded size limit")
	}
	return nil
}
func checksum(manifest []byte, asset string) (string, error) {
	found := ""
	for _, line := range strings.Split(string(manifest), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("duplicate release checksum")
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != sha256.Size {
			return "", fmt.Errorf("invalid release checksum")
		}
		found = strings.ToLower(fields[0])
	}
	if found == "" {
		return "", fmt.Errorf("no checksum for %s", asset)
	}
	return found, nil
}
func extract(archive io.Reader, candidate string) error {
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if (header.Name != "observational-memory" && header.Name != "LICENSE") || header.Typeflag != tar.TypeReg {
			return fmt.Errorf("unexpected archive member %q", header.Name)
		}
		if header.Name == "LICENSE" {
			continue
		}
		if found {
			return fmt.Errorf("duplicate executable in archive")
		}
		found = true
		if header.Size > 64<<20 {
			return fmt.Errorf("executable exceeds size limit")
		}
		file, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if err != nil {
			return err
		}
		_, err = io.Copy(file, reader)
		if err == nil {
			err = file.Sync()
		}
		if err = errors.Join(err, file.Close()); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("archive has no executable")
	}
	return nil
}
