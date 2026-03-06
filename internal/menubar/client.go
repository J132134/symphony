package menubar

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"symphony/internal/status"
)

type Options struct {
	BaseURL      string
	PollInterval time.Duration
}

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:7777"
	}
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

func (c *Client) Summary() (status.Summary, error) {
	var summary status.Summary

	resp, err := c.http.Get(c.baseURL + "/api/v1/summary")
	if err != nil {
		return summary, fmt.Errorf("fetch summary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return summary, fmt.Errorf("fetch summary: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		return summary, fmt.Errorf("decode summary: %w", err)
	}
	return summary, nil
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

func (c *Client) DashboardURL() string {
	return c.baseURL
}
