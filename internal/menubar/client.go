package menubar

import (
	"time"

	"symphony/internal/status"
)

type Options struct {
	BaseURL      string
	PollInterval time.Duration
}

type Client struct {
	client *status.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		client: status.NewClient(baseURL),
	}
}

func (c *Client) Summary() (status.Summary, error) {
	return c.client.Summary()
}

func (c *Client) Refresh() error {
	return c.client.Refresh()
}
