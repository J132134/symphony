package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// CheckResult holds the result of a release check.
type CheckResult struct {
	Available   bool
	CurrentVer  string
	LatestVer   string
	DownloadURL string
}

// Checker checks GitHub Releases for updates and downloads assets.
type Checker struct {
	Owner   string
	Repo    string
	Asset   string
	BaseURL string // override for tests; defaults to "https://api.github.com"
	client  *http.Client
}

func (c *Checker) apiBase() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://api.github.com"
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func (c *Checker) httpClient() *http.Client {
	if c != nil && c.client != nil {
		return c.client
	}
	return httpClient
}

// Check fetches the latest release and reports whether an update is available.
// Returns immediately without checking if currentVersion == "dev".
func (c *Checker) Check(currentVersion string) (*CheckResult, error) {
	if currentVersion == "dev" {
		return &CheckResult{CurrentVer: currentVersion}, nil
	}

	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", c.apiBase(), c.Owner, c.Repo)
	resp, err := c.httpClient().Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch latest release: HTTP %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
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
	if release.TagName == currentVersion {
		return result, nil
	}
	for _, asset := range release.Assets {
		if asset.Name == c.Asset {
			result.Available = true
			result.DownloadURL = asset.BrowserDownloadURL
			return result, nil
		}
	}
	return result, nil
}

// Download fetches the asset at url into a temp file and returns its path.
// The caller is responsible for removing the file.
func (c *Checker) Download(url string) (string, error) {
	resp, err := c.httpClient().Get(url)
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

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write asset: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("close temp file: %w", err)
	}
	return f.Name(), nil
}
