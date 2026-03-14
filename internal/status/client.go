package status

import (
	"bytes"
	"context"
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
	return c.SummaryCtx(context.Background())
}

func (c *Client) SummaryCtx(ctx context.Context) (Summary, error) {
	var summary Summary
	if err := c.getJSON(ctx, "/api/v1/summary", &summary); err != nil {
		return summary, err
	}
	return summary, nil
}

func (c *Client) Projects() ([]ProjectSummary, error) {
	var projects []ProjectSummary
	if err := c.getJSON(context.Background(), "/api/v1/projects", &projects); err != nil {
		return nil, err
	}
	return projects, nil
}

func (c *Client) Refresh() error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.baseURL+"/api/v1/refresh", bytes.NewReader(nil))
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

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", path, err)
	}
	resp, err := c.http.Do(req)
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
