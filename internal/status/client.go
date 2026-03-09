package status

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "http://127.0.0.1:7777"

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

func (c *Client) Summary() (Summary, error) {
	var summary Summary
	if err := c.getJSON("/api/v1/summary", &summary); err != nil {
		return summary, err
	}
	return summary, nil
}

func (c *Client) Projects() ([]ProjectSummary, error) {
	var projects []ProjectSummary
	if err := c.getJSON("/api/v1/projects", &projects); err != nil {
		return nil, err
	}
	return projects, nil
}

func (c *Client) Refresh() error {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/v1/refresh", bytes.NewReader(nil))
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("refresh: status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) getJSON(path string, v any) error {
	resp, err := c.http.Get(c.baseURL + path)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
