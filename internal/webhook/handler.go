package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

type LinearEvent struct {
	Action    string         `json:"action"`
	Type      string         `json:"type"`
	Data      map[string]any `json:"data"`
	WebhookID string         `json:"webhookId"`
	CreatedAt string         `json:"createdAt"`
}

type Refresher interface {
	TriggerRefresh(ctx context.Context)
}

type Handler struct {
	signingSecret string
	refresher     Refresher
}

func NewHandler(signingSecret string, refresher Refresher) *Handler {
	return &Handler{
		signingSecret: strings.TrimSpace(signingSecret),
		refresher:     refresher,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("webhook.read_failed", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	if h.signingSecret != "" && !VerifySignature(body, r.Header.Get("Linear-Signature"), h.signingSecret) {
		slog.Warn("webhook.signature_invalid", "event", r.Header.Get("Linear-Event"))
		w.WriteHeader(http.StatusOK)
		return
	}

	var event LinearEvent
	if err := json.Unmarshal(body, &event); err != nil {
		slog.Warn("webhook.invalid_payload", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	slog.Info(
		"webhook.received",
		"type", event.Type,
		"action", event.Action,
		"webhook_id", event.WebhookID,
	)

	if event.Type == "Issue" && h.refresher != nil {
		slog.Info("webhook.refresh_triggered", "type", event.Type, "action", event.Action, "webhook_id", event.WebhookID)
		h.refresher.TriggerRefresh(r.Context())
	}

	w.WriteHeader(http.StatusOK)
}

func VerifySignature(body []byte, signature, secret string) bool {
	if signature == "" || secret == "" {
		return false
	}

	headerSignature, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}

	computedSignature := hmac.New(sha256.New, []byte(secret))
	computedSignature.Write(body)
	return hmac.Equal(computedSignature.Sum(nil), headerSignature)
}
