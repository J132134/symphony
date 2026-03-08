package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type releaseAsset struct {
	name   string
	body   []byte
	status int
}

func makeReleaseServer(t *testing.T, tagName string, assets ...releaseAsset) *httptest.Server {
	t.Helper()

	assetMap := make(map[string]releaseAsset, len(assets))
	for _, asset := range assets {
		assetMap[asset.name] = asset
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			payload := map[string]any{
				"tag_name": tagName,
				"assets":   []any{},
			}
			list := payload["assets"].([]any)
			for _, asset := range assets {
				list = append(list, map[string]any{
					"name":                 asset.name,
					"browser_download_url": srv.URL + "/assets/" + asset.name,
				})
			}
			payload["assets"] = list
			_ = json.NewEncoder(w).Encode(payload)
		default:
			name := strings.TrimPrefix(r.URL.Path, "/assets/")
			asset, ok := assetMap[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			status := asset.status
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_, _ = w.Write(asset.body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func checksumBody(content []byte) []byte {
	sum := sha256.Sum256(content)
	return []byte(hex.EncodeToString(sum[:]) + "  symphony-darwin-arm64\n")
}

func TestCheckerCheckSkipsDevVersion(t *testing.T) {
	t.Parallel()

	binary := []byte("fake binary")
	srv := makeReleaseServer(t, "v1.1.0",
		releaseAsset{name: "a", body: binary},
		releaseAsset{name: "a.sha256", body: checksumBody(binary)},
	)
	c := &Checker{Owner: "o", Repo: "r", Asset: "a", BaseURL: srv.URL}

	result, err := c.Check("dev")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Available {
		t.Fatal("Check() Available = true, want false for dev version")
	}
}

func TestCheckerCheckSkipsNonReleaseCurrentVersion(t *testing.T) {
	t.Parallel()

	binary := []byte("fake binary")
	srv := makeReleaseServer(t, "v1.1.0",
		releaseAsset{name: "symphony-darwin-arm64", body: binary},
		releaseAsset{name: "symphony-darwin-arm64.sha256", body: checksumBody(binary)},
	)
	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}

	result, err := c.Check("abc1234")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Available {
		t.Fatal("Check() Available = true, want false for non-release current version")
	}
}

func TestCheckerCheckNoUpdateWhenVersionMatches(t *testing.T) {
	t.Parallel()

	binary := []byte("fake binary")
	srv := makeReleaseServer(t, "v1.0.0",
		releaseAsset{name: "symphony-darwin-arm64", body: binary},
		releaseAsset{name: "symphony-darwin-arm64.sha256", body: checksumBody(binary)},
	)
	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}

	result, err := c.Check("v1.0.0")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Available {
		t.Fatal("Check() Available = true, want false when version matches")
	}
}

func TestCheckerCheckRejectsDowngrade(t *testing.T) {
	t.Parallel()

	binary := []byte("fake binary")
	srv := makeReleaseServer(t, "v0.9.0",
		releaseAsset{name: "symphony-darwin-arm64", body: binary},
		releaseAsset{name: "symphony-darwin-arm64.sha256", body: checksumBody(binary)},
	)
	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}

	result, err := c.Check("v1.0.0")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Available {
		t.Fatal("Check() Available = true, want false for downgrade release")
	}
}

func TestCheckerCheckUpdateAvailable(t *testing.T) {
	t.Parallel()

	binary := []byte("binary-v1.1.0")
	srv := makeReleaseServer(t, "v1.1.0",
		releaseAsset{name: "symphony-darwin-arm64", body: binary},
		releaseAsset{name: "symphony-darwin-arm64.sha256", body: checksumBody(binary)},
	)
	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}

	result, err := c.Check("v1.0.0")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.Available {
		t.Fatal("Check() Available = false, want true when newer version exists")
	}
	if want := srv.URL + "/assets/symphony-darwin-arm64"; result.DownloadURL != want {
		t.Fatalf("Check() DownloadURL = %q, want %q", result.DownloadURL, want)
	}
	if want := srv.URL + "/assets/symphony-darwin-arm64.sha256"; result.ChecksumURL != want {
		t.Fatalf("Check() ChecksumURL = %q, want %q", result.ChecksumURL, want)
	}
	if result.LatestVer != "v1.1.0" {
		t.Fatalf("Check() LatestVer = %q, want %q", result.LatestVer, "v1.1.0")
	}
}

func TestCheckerCheckAssetMissingNoUpdate(t *testing.T) {
	t.Parallel()

	binary := []byte("linux")
	srv := makeReleaseServer(t, "v1.1.0",
		releaseAsset{name: "symphony-linux-amd64", body: binary},
		releaseAsset{name: "symphony-linux-amd64.sha256", body: checksumBody(binary)},
	)
	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}

	result, err := c.Check("v1.0.0")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Available {
		t.Fatal("Check() Available = true, want false when asset name doesn't match")
	}
}

func TestCheckerCheckMissingChecksumFailsClosed(t *testing.T) {
	t.Parallel()

	binary := []byte("binary-v1.1.0")
	srv := makeReleaseServer(t, "v1.1.0", releaseAsset{name: "symphony-darwin-arm64", body: binary})
	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}

	_, err := c.Check("v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "missing checksum asset") {
		t.Fatalf("Check() error = %v, want missing checksum asset", err)
	}
}

func TestCheckerCheckRejectsInvalidLatestTag(t *testing.T) {
	t.Parallel()

	binary := []byte("binary-v1.1.0")
	srv := makeReleaseServer(t, "latest",
		releaseAsset{name: "symphony-darwin-arm64", body: binary},
		releaseAsset{name: "symphony-darwin-arm64.sha256", body: checksumBody(binary)},
	)
	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}

	_, err := c.Check("v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "not canonical semver") {
		t.Fatalf("Check() error = %v, want canonical semver failure", err)
	}
}

func TestCheckerCheckSkipsPrerelease(t *testing.T) {
	t.Parallel()

	binary := []byte("binary-v1.1.0")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name":   "v1.1.0",
				"prerelease": true,
				"assets": []any{
					map[string]any{
						"name":                 "symphony-darwin-arm64",
						"browser_download_url": srv.URL + "/assets/symphony-darwin-arm64",
					},
					map[string]any{
						"name":                 "symphony-darwin-arm64.sha256",
						"browser_download_url": srv.URL + "/assets/symphony-darwin-arm64.sha256",
					},
				},
			})
		case "/assets/symphony-darwin-arm64":
			_, _ = w.Write(binary)
		case "/assets/symphony-darwin-arm64.sha256":
			_, _ = w.Write(checksumBody(binary))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}
	result, err := c.Check("v1.0.0")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Available {
		t.Fatal("Check() Available = true, want false for prerelease")
	}
}

func TestCheckerDownload(t *testing.T) {
	t.Parallel()

	content := []byte("fake binary content")
	srv := makeReleaseServer(t, "v1.1.0",
		releaseAsset{name: "symphony-darwin-arm64", body: content},
		releaseAsset{name: "symphony-darwin-arm64.sha256", body: checksumBody(content)},
	)

	c := &Checker{BaseURL: srv.URL}
	path, err := c.Download(
		srv.URL+"/assets/symphony-darwin-arm64",
		srv.URL+"/assets/symphony-darwin-arm64.sha256",
	)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != string(content) {
		t.Fatalf("Download() content = %q, want %q", data, content)
	}
}

func TestCheckerDownloadRejectsChecksumMismatch(t *testing.T) {
	t.Parallel()

	content := []byte("fake binary content")
	srv := makeReleaseServer(t, "v1.1.0",
		releaseAsset{name: "symphony-darwin-arm64", body: content},
		releaseAsset{name: "symphony-darwin-arm64.sha256", body: []byte(strings.Repeat("0", 64) + "\n")},
	)

	c := &Checker{BaseURL: srv.URL}
	path, err := c.Download(
		srv.URL+"/assets/symphony-darwin-arm64",
		srv.URL+"/assets/symphony-darwin-arm64.sha256",
	)
	if err == nil || !strings.Contains(err.Error(), "verify asset checksum") {
		t.Fatalf("Download() error = %v, want checksum verification failure", err)
	}
	if path != "" {
		t.Fatalf("Download() path = %q, want empty path on failure", path)
	}
}

func TestParseSHA256ChecksumRejectsInvalidBody(t *testing.T) {
	t.Parallel()

	if _, err := parseSHA256Checksum([]byte("not-a-checksum")); err == nil {
		t.Fatal("parseSHA256Checksum() error = nil, want invalid checksum error")
	}
}
