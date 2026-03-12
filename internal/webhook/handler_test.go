package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type recordingRefresher struct {
	mu    sync.Mutex
	count int
}

func (r *recordingRefresher) TriggerRefresh(context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
}

func (r *recordingRefresher) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func TestHandlerServeHTTP(t *testing.T) {
	tests := []struct {
		name          string
		method        string
		secret        string
		signature     string
		body          string
		wantStatus    int
		wantRefreshes int
	}{
		{
			name:          "valid issue event with signature",
			method:        http.MethodPost,
			secret:        "top-secret",
			body:          `{"action":"update","type":"Issue","data":{"id":"issue-1"}}`,
			wantStatus:    http.StatusOK,
			wantRefreshes: 1,
		},
		{
			name:          "invalid signature",
			method:        http.MethodPost,
			secret:        "top-secret",
			signature:     "deadbeef",
			body:          `{"action":"update","type":"Issue","data":{"id":"issue-1"}}`,
			wantStatus:    http.StatusOK,
			wantRefreshes: 0,
		},
		{
			name:          "empty signing secret allows development mode",
			method:        http.MethodPost,
			body:          `{"action":"update","type":"Issue","data":{"id":"issue-1"}}`,
			wantStatus:    http.StatusOK,
			wantRefreshes: 1,
		},
		{
			name:          "non issue event",
			method:        http.MethodPost,
			body:          `{"action":"update","type":"Comment","data":{"id":"comment-1"}}`,
			wantStatus:    http.StatusOK,
			wantRefreshes: 0,
		},
		{
			name:          "invalid json",
			method:        http.MethodPost,
			body:          `{"action":"update"`,
			wantStatus:    http.StatusOK,
			wantRefreshes: 0,
		},
		{
			name:          "rejects get method",
			method:        http.MethodGet,
			body:          `{"action":"update","type":"Issue"}`,
			wantStatus:    http.StatusMethodNotAllowed,
			wantRefreshes: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refresher := &recordingRefresher{}
			handler := NewHandler(tc.secret, refresher)

			req := httptest.NewRequest(tc.method, "/webhook/linear", strings.NewReader(tc.body))
			if tc.secret != "" && tc.signature == "" && tc.method == http.MethodPost {
				req.Header.Set("Linear-Signature", signPayload(tc.body, tc.secret))
			}
			if tc.signature != "" {
				req.Header.Set("Linear-Signature", tc.signature)
			}

			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)

			if got := res.Code; got != tc.wantStatus {
				t.Fatalf("status = %d, want %d", got, tc.wantStatus)
			}
			if got := refresher.Count(); got != tc.wantRefreshes {
				t.Fatalf("refresh count = %d, want %d", got, tc.wantRefreshes)
			}
		})
	}
}

func TestVerifySignature(t *testing.T) {
	t.Parallel()

	body := []byte(`{"type":"Issue"}`)
	secret := "top-secret"
	signature := signPayload(string(body), secret)

	if !VerifySignature(body, signature, secret) {
		t.Fatal("VerifySignature() = false, want true")
	}
	if VerifySignature(body, "deadbeef", secret) {
		t.Fatal("VerifySignature() should reject invalid signatures")
	}
}

func signPayload(body, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}
