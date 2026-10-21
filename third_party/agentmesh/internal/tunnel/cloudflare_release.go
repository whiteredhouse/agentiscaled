// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tunnel

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// CloudflaredVersion is the cloudflared release this build knows how to
// install. Bump it together with cloudflaredAssets: the digests are the only
// thing standing between "download from GitHub" and "run whatever answered".
const CloudflaredVersion = "2026.9.1"

// CloudflaredLicenseURL is what a user accepts by installing cloudflared.
const CloudflaredLicenseURL = "https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/downloads/license/"

// DefaultCloudflaredReleaseURL is the GitHub releases download prefix.
const DefaultCloudflaredReleaseURL = "https://github.com/cloudflare/cloudflared/releases/download/"

// maxCloudflaredDownload caps the asset size; current builds are ~40 MiB.
const maxCloudflaredDownload = 128 << 20

// releaseAsset pins one downloadable build. AssetSHA256 covers the bytes
// fetched; BinarySHA256 covers the executable that ends up on disk (equal for
// raw binaries, the inner file for tarballs) and is what gets re-checked
// before every launch.
type releaseAsset struct {
	Name         string
	AssetSHA256  string
	BinarySHA256 string
	Tarball      bool
}

// cloudflaredAssets maps GOOS/GOARCH to the pinned asset. Verified against
// the GitHub asset digests of CloudflaredVersion; the macOS entries also
// match the per-binary checksums Cloudflare lists in the release notes.
var cloudflaredAssets = map[string]releaseAsset{
	"linux/amd64": {
		Name:         "cloudflared-linux-amd64",
		AssetSHA256:  "03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc",
		BinarySHA256: "03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc",
	},
	"linux/arm64": {
		Name:         "cloudflared-linux-arm64",
		AssetSHA256:  "3d97437c71848bd8df68041e12436b484a661d95073ea1937f01a845ce88faa3",
		BinarySHA256: "3d97437c71848bd8df68041e12436b484a661d95073ea1937f01a845ce88faa3",
	},
	"linux/arm": {
		Name:         "cloudflared-linux-arm",
		AssetSHA256:  "093ffa3638ab2b636de63c43a8c68f96a69cf71f9699dd8277a91b160b0f4fc0",
		BinarySHA256: "093ffa3638ab2b636de63c43a8c68f96a69cf71f9699dd8277a91b160b0f4fc0",
	},
	"linux/386": {
		Name:         "cloudflared-linux-386",
		AssetSHA256:  "5d66134cf7646cb98f33aeee7bcc8b97d8feacd76db279f5903f9585226e0922",
		BinarySHA256: "5d66134cf7646cb98f33aeee7bcc8b97d8feacd76db279f5903f9585226e0922",
	},
	"darwin/amd64": {
		Name:         "cloudflared-darwin-amd64.tgz",
		AssetSHA256:  "ff0d3b51d5ff70eceef89d6b32145fee985018a2174596a5dbe405e2766e2ac4",
		BinarySHA256: "1ea07ae775b03236bd6be18ca1848d6bdc4af2f4f3bce398823b5a36e5761b75",
		Tarball:      true,
	},
	"darwin/arm64": {
		Name:         "cloudflared-darwin-arm64.tgz",
		AssetSHA256:  "c27ab8fd0aa489449e3d201eb02f957ef460a13b613662928b1b23394bf1bcfe",
		BinarySHA256: "9a0b19f67dc7a3011bc6b972c7ce06a5fcea8784ac6bd599ffa382ea4aeb5a6e",
		Tarball:      true,
	},
}

// ErrCloudflaredUnavailable is returned when no usable cloudflared exists and
// the provider was not allowed to install one.
var ErrCloudflaredUnavailable = errors.New("cloudflared not found")

// pinnedAsset returns the asset for this platform, if there is one.
func pinnedAsset() (releaseAsset, bool) {
	a, ok := cloudflaredAssets[runtime.GOOS+"/"+runtime.GOARCH]
	return a, ok
}

// installedCloudflared is the path a downloaded binary lives at.
func installedCloudflared(dir string) string {
	return filepath.Join(dir, "cloudflared")
}

// verifyBinary reports whether the file at path hashes to want.
func verifyBinary(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}

// downloadCloudflared fetches the pinned asset for this platform into dir,
// verifying the asset digest before anything is extracted and the binary
// digest before the file is put in place. Nothing is executed here.
func downloadCloudflared(ctx context.Context, client *http.Client, baseURL, dir string) (string, error) {
	asset, ok := pinnedAsset()
	if !ok {
		return "", fmt.Errorf("no pinned cloudflared build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create %s: %w", dir, err)
	}
	url := baseURL + CloudflaredVersion + "/" + asset.Name
	logger.Infof("Downloading cloudflared %s from %s", CloudflaredVersion, url)

	// Whole asset to a temp file first: the digest must be checked before a
	// single byte is interpreted as a tarball or an executable.
	tmp, err := os.CreateTemp(dir, ".cloudflared-download-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := fetchVerified(ctx, client, url, tmp, asset.AssetSHA256); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		return "", err
	}

	bin, err := os.CreateTemp(dir, ".cloudflared-bin-*")
	if err != nil {
		_ = tmp.Close()
		return "", err
	}
	binPath := bin.Name()
	defer func() { _ = os.Remove(binPath) }()
	var src io.Reader = tmp
	var gzCloser io.Closer
	if asset.Tarball {
		src, gzCloser, err = tarballEntry(tmp, "cloudflared")
		if err != nil {
			_ = tmp.Close()
			_ = bin.Close()
			return "", err
		}
		defer func() { _ = gzCloser.Close() }()
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(bin, h), io.LimitReader(src, maxCloudflaredDownload+1))
	if err != nil {
		_ = tmp.Close()
		_ = bin.Close()
		return "", fmt.Errorf("failed to write cloudflared: %w", err)
	}
	_ = tmp.Close()
	if err := bin.Close(); err != nil {
		return "", err
	}
	if n > maxCloudflaredDownload {
		return "", fmt.Errorf("extracted cloudflared exceeds %d bytes", maxCloudflaredDownload)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != asset.BinarySHA256 {
		return "", fmt.Errorf("cloudflared binary digest mismatch: got %s, want %s", got, asset.BinarySHA256)
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return "", err
	}
	dest := installedCloudflared(dir)
	if err := os.Rename(binPath, dest); err != nil {
		return "", fmt.Errorf("failed to install cloudflared: %w", err)
	}
	return dest, nil
}

// fetchVerified streams url into w and fails unless the bytes hash to want.
// The caller must discard w's contents on error.
func fetchVerified(ctx context.Context, client *http.Client, url string, w io.Writer, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s returned %s", url, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, maxCloudflaredDownload+1))
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	if n > maxCloudflaredDownload {
		return fmt.Errorf("download exceeds %d bytes", maxCloudflaredDownload)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("cloudflared asset digest mismatch: got %s, want %s (refusing to install)", got, want)
	}
	return nil
}

// tarballEntry positions a reader on the named regular file inside a
// gzip-compressed tarball.
func tarballEntry(r io.Reader, name string) (io.Reader, io.Closer, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid cloudflared tarball: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			_ = gz.Close()
			return nil, nil, fmt.Errorf("cloudflared tarball has no %q entry", name)
		}
		if err != nil {
			_ = gz.Close()
			return nil, nil, fmt.Errorf("invalid cloudflared tarball: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == name {
			return tr, gz, nil
		}
	}
}

// defaultDownloadClient bounds a ~40 MiB fetch generously.
func defaultDownloadClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute}
}
