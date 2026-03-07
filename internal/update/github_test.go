package update

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func makeReleaseServer(t *testing.T, tagName string, assetName string, downloadURL string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tagName,
			"assets": []any{
				map[string]any{
					"name":                 assetName,
					"browser_download_url": downloadURL,
				},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckerCheckSkipsDevVersion(t *testing.T) {
	t.Parallel()

	c := &Checker{Owner: "o", Repo: "r", Asset: "a"}
	result, err := c.Check("dev")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Available {
		t.Fatal("Check() Available = true, want false for dev version")
	}
}

func TestCheckerCheckNoUpdateWhenVersionMatches(t *testing.T) {
	t.Parallel()

	srv := makeReleaseServer(t, "v1.0.0", "symphony-darwin-arm64", "http://example.com/binary")
	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}

	result, err := c.Check("v1.0.0")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Available {
		t.Fatal("Check() Available = true, want false when version matches")
	}
}

func TestCheckerCheckUpdateAvailable(t *testing.T) {
	t.Parallel()

	const wantURL = "http://example.com/binary-v1.1.0"
	srv := makeReleaseServer(t, "v1.1.0", "symphony-darwin-arm64", wantURL)
	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}

	result, err := c.Check("v1.0.0")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.Available {
		t.Fatal("Check() Available = false, want true when newer version exists")
	}
	if result.DownloadURL != wantURL {
		t.Fatalf("Check() DownloadURL = %q, want %q", result.DownloadURL, wantURL)
	}
	if result.LatestVer != "v1.1.0" {
		t.Fatalf("Check() LatestVer = %q, want %q", result.LatestVer, "v1.1.0")
	}
}

func TestCheckerCheckAssetMissingNoUpdate(t *testing.T) {
	t.Parallel()

	srv := makeReleaseServer(t, "v1.1.0", "symphony-linux-amd64", "http://example.com/binary")
	c := &Checker{Owner: "o", Repo: "r", Asset: "symphony-darwin-arm64", BaseURL: srv.URL}

	result, err := c.Check("v1.0.0")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Available {
		t.Fatal("Check() Available = true, want false when asset name doesn't match")
	}
}

func TestCheckerDownload(t *testing.T) {
	t.Parallel()

	content := []byte("fake binary content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	t.Cleanup(srv.Close)

	c := &Checker{}
	path, err := c.Download(srv.URL)
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
