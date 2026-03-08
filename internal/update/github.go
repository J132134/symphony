package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// CheckResult holds the result of a release check.
type CheckResult struct {
	Available   bool
	CurrentVer  string
	LatestVer   string
	DownloadURL string
	ChecksumURL string
}

// Checker checks GitHub Releases for updates and downloads assets.
type Checker struct {
	Owner   string
	Repo    string
	Asset   string
	BaseURL string // override for tests; defaults to "https://api.github.com"
}

func (c *Checker) apiBase() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://api.github.com"
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Check fetches the latest release and reports whether an update is available.
// Returns immediately without checking if currentVersion == "dev".
func (c *Checker) Check(currentVersion string) (*CheckResult, error) {
	if currentVersion == "dev" {
		return &CheckResult{CurrentVer: currentVersion}, nil
	}

	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", c.apiBase(), c.Owner, c.Repo)
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch latest release: HTTP %d", resp.StatusCode)
	}

	var release struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}

	result := &CheckResult{
		CurrentVer: currentVersion,
		LatestVer:  release.TagName,
	}
	if release.Draft || release.Prerelease {
		return result, nil
	}

	current, ok := parseCanonicalVersion(currentVersion)
	if !ok {
		return result, nil
	}
	latest, ok := parseCanonicalVersion(release.TagName)
	if !ok {
		return nil, fmt.Errorf("latest release tag %q is not canonical semver", release.TagName)
	}
	if compareCanonicalVersion(latest, current) <= 0 {
		return result, nil
	}

	checksumAsset := c.Asset + ".sha256"
	for _, asset := range release.Assets {
		switch asset.Name {
		case c.Asset:
			if err := c.validateDownloadURL(asset.BrowserDownloadURL); err != nil {
				return nil, fmt.Errorf("release asset url for %q: %w", asset.Name, err)
			}
			result.DownloadURL = asset.BrowserDownloadURL
		case checksumAsset:
			if err := c.validateDownloadURL(asset.BrowserDownloadURL); err != nil {
				return nil, fmt.Errorf("release checksum url for %q: %w", asset.Name, err)
			}
			result.ChecksumURL = asset.BrowserDownloadURL
		}
	}
	if result.DownloadURL == "" {
		return result, nil
	}
	if result.ChecksumURL == "" {
		return nil, fmt.Errorf("release %s missing checksum asset %q", release.TagName, checksumAsset)
	}

	result.Available = true
	return result, nil
}

// Download fetches the asset at url into a temp file and returns its path.
// The caller is responsible for removing the file.
func (c *Checker) Download(downloadURL, checksumURL string) (string, error) {
	expected, err := c.fetchChecksum(checksumURL)
	if err != nil {
		return "", err
	}

	resp, err := httpClient.Get(downloadURL)
	if err != nil {
		return "", fmt.Errorf("download asset: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download asset: HTTP %d", resp.StatusCode)
	}

	f, err := os.CreateTemp("", "symphony-update-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), resp.Body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write asset: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("close temp file: %w", err)
	}

	actual := hasher.Sum(nil)
	if !bytes.Equal(actual, expected[:]) {
		os.Remove(f.Name())
		return "", fmt.Errorf("verify asset checksum: got %x want %x", actual, expected)
	}

	return f.Name(), nil
}

func (c *Checker) fetchChecksum(checksumURL string) ([32]byte, error) {
	var zero [32]byte

	resp, err := httpClient.Get(checksumURL)
	if err != nil {
		return zero, fmt.Errorf("download checksum: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("download checksum: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return zero, fmt.Errorf("read checksum: %w", err)
	}

	sum, err := parseSHA256Checksum(body)
	if err != nil {
		return zero, err
	}
	return sum, nil
}

func parseSHA256Checksum(body []byte) ([32]byte, error) {
	var sum [32]byte

	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return sum, fmt.Errorf("parse checksum: empty body")
	}
	if len(fields[0]) != sha256.Size*2 {
		return sum, fmt.Errorf("parse checksum: invalid sha256 length %d", len(fields[0]))
	}

	raw, err := hex.DecodeString(fields[0])
	if err != nil {
		return sum, fmt.Errorf("parse checksum: %w", err)
	}
	copy(sum[:], raw)
	return sum, nil
}

func (c *Checker) validateDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("url must include scheme and host")
	}
	if c.BaseURL == "" && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("url must use https")
	}
	return nil
}

type canonicalVersion struct {
	major int
	minor int
	patch int
}

func parseCanonicalVersion(v string) (canonicalVersion, bool) {
	if !strings.HasPrefix(v, "v") {
		return canonicalVersion{}, false
	}
	parts := strings.Split(v[1:], ".")
	if len(parts) != 3 {
		return canonicalVersion{}, false
	}

	nums := [3]int{}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return canonicalVersion{}, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return canonicalVersion{}, false
		}
		nums[i] = n
	}
	return canonicalVersion{major: nums[0], minor: nums[1], patch: nums[2]}, true
}

func compareCanonicalVersion(a, b canonicalVersion) int {
	switch {
	case a.major != b.major:
		return cmpInt(a.major, b.major)
	case a.minor != b.minor:
		return cmpInt(a.minor, b.minor)
	default:
		return cmpInt(a.patch, b.patch)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
