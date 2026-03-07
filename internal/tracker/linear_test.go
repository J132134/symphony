package tracker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeIssueCapturesLatestComment(t *testing.T) {
	t.Parallel()

	issue := normalizeIssue(map[string]any{
		"id":          "issue-1",
		"identifier":  "J-27",
		"title":       "retry",
		"description": "",
		"state":       map[string]any{"name": "In Progress"},
		"comments": map[string]any{
			"nodes": []any{
				map[string]any{
					"id":        "comment-1",
					"body":      "<!-- symphony:retry-abandoned -->\nbody",
					"createdAt": "2026-03-06T00:00:00Z",
					"updatedAt": "2026-03-06T00:00:01Z",
				},
			},
		},
		"labels":    map[string]any{"nodes": []any{}},
		"relations": map[string]any{"nodes": []any{}},
		"createdAt": "2026-03-05T00:00:00Z",
		"updatedAt": "2026-03-06T00:00:01Z",
	})

	if issue.LastComment == nil {
		t.Fatal("LastComment should be captured")
	}
	if len(issue.Comments) != 1 {
		t.Fatalf("len(Comments) = %d, want 1", len(issue.Comments))
	}
	if got := issue.Comments[0].ID; got != "comment-1" {
		t.Fatalf("Comments[0].ID = %q, want comment-1", got)
	}
	if !strings.Contains(issue.LastComment.Body, "retry-abandoned") {
		t.Fatalf("LastComment.Body = %q, want retry-abandoned marker", issue.LastComment.Body)
	}
	if issue.LastComment.UpdatedAt == nil {
		t.Fatal("LastComment.UpdatedAt should be parsed")
	}
}

func TestNormalizeIssueCapturesBlockedByRelations(t *testing.T) {
	t.Parallel()

	issue := normalizeIssue(map[string]any{
		"id":          "issue-1",
		"identifier":  "J-33",
		"title":       "blocked issue",
		"description": "",
		"state":       map[string]any{"name": "Todo"},
		"comments":    map[string]any{"nodes": []any{}},
		"labels":      map[string]any{"nodes": []any{}},
		"relations": map[string]any{
			"nodes": []any{
				map[string]any{
					"type": "blocked_by",
					"relatedIssue": map[string]any{
						"id":         "issue-31",
						"identifier": "J-31",
						"state":      map[string]any{"name": "In Progress"},
					},
				},
				map[string]any{
					"type": "blocks",
					"relatedIssue": map[string]any{
						"id":         "issue-99",
						"identifier": "J-99",
						"state":      map[string]any{"name": "Todo"},
					},
				},
			},
		},
		"createdAt": "2026-03-05T00:00:00Z",
		"updatedAt": "2026-03-06T00:00:01Z",
	})

	if len(issue.BlockedBy) != 1 {
		t.Fatalf("len(BlockedBy) = %d, want 1", len(issue.BlockedBy))
	}
	if got := issue.BlockedBy[0]; got.Identifier != "J-31" || got.State != "In Progress" {
		t.Fatalf("BlockedBy[0] = %#v, want identifier/state for J-31 in progress", got)
	}
}

func TestAddComment(t *testing.T) {
	t.Parallel()

	var authHeader string
	var capturedBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		capturedBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"commentCreate":{"success":true}}}`))
	}))
	defer srv.Close()

	client, err := NewLinearClient("token", srv.URL, "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	if err := client.AddComment(context.Background(), "issue-1", "hello"); err != nil {
		t.Fatalf("AddComment() error = %v", err)
	}
	if authHeader != "token" {
		t.Fatalf("Authorization header = %q, want %q", authHeader, "token")
	}
	if !strings.Contains(capturedBody, "commentCreate") {
		t.Fatalf("request body missing commentCreate mutation: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"issueId":"issue-1"`) {
		t.Fatalf("request body missing issueId: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"body":"hello"`) {
		t.Fatalf("request body missing body: %s", capturedBody)
	}
}

func TestAddCommentRejectsEmptyBody(t *testing.T) {
	t.Parallel()

	client, err := NewLinearClient("token", "https://example.com", "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	err = client.AddComment(context.Background(), "issue-1", " \n\t ")
	if err == nil {
		t.Fatal("AddComment() returned nil error for blank body")
	}
	if !strings.Contains(err.Error(), "comment body is required") {
		t.Fatalf("AddComment() error = %v, want comment body is required", err)
	}
}

func TestAddCommentRejectsEmptyIssueID(t *testing.T) {
	t.Parallel()

	client, err := NewLinearClient("token", "https://example.com", "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	err = client.AddComment(context.Background(), "", "hello")
	if err == nil {
		t.Fatal("AddComment() returned nil error for empty issue ID")
	}
	if !strings.Contains(err.Error(), "issue ID is required") {
		t.Fatalf("AddComment() error = %v, want issue ID is required", err)
	}
}

func TestAddCommentReturnsErrorOnUnsuccessfulMutation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"commentCreate":{"success":false}}}`))
	}))
	defer srv.Close()

	client, err := NewLinearClient("token", srv.URL, "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	err = client.AddComment(context.Background(), "issue-1", "hello")
	if err == nil {
		t.Fatal("AddComment() returned nil error for unsuccessful mutation")
	}
	if !strings.Contains(err.Error(), "commentCreate unsuccessful") {
		t.Fatalf("AddComment() error = %v, want commentCreate unsuccessful", err)
	}
}

func TestAddCommentReturnsGraphQLError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"permission denied"}]}`))
	}))
	defer srv.Close()

	client, err := NewLinearClient("token", srv.URL, "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	err = client.AddComment(context.Background(), "issue-1", "hello")
	if err == nil {
		t.Fatal("AddComment() returned nil error for GraphQL error response")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("AddComment() error = %v, want GraphQL error message", err)
	}
}

func TestAddCommentReturnsHTTPStatusError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":[{"message":"forbidden"}]}`, http.StatusForbidden)
	}))
	defer srv.Close()

	client, err := NewLinearClient("token", srv.URL, "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	err = client.AddComment(context.Background(), "issue-1", "hello")
	if err == nil {
		t.Fatal("AddComment() returned nil error for HTTP error response")
	}
	if !strings.Contains(err.Error(), "Linear API status 403") {
		t.Fatalf("AddComment() error = %v, want HTTP status message", err)
	}
}

func TestUpdateComment(t *testing.T) {
	t.Parallel()

	var authHeader string
	var capturedBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		capturedBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"commentUpdate":{"success":true}}}`))
	}))
	defer srv.Close()

	client, err := NewLinearClient("token", srv.URL, "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	if err := client.UpdateComment(context.Background(), "comment-1", "updated"); err != nil {
		t.Fatalf("UpdateComment() error = %v", err)
	}
	if authHeader != "token" {
		t.Fatalf("Authorization header = %q, want %q", authHeader, "token")
	}
	if !strings.Contains(capturedBody, "commentUpdate") {
		t.Fatalf("request body missing commentUpdate mutation: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"id":"comment-1"`) {
		t.Fatalf("request body missing comment id: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"body":"updated"`) {
		t.Fatalf("request body missing body: %s", capturedBody)
	}
}

func TestUpdateCommentRejectsEmptyCommentID(t *testing.T) {
	t.Parallel()

	client, err := NewLinearClient("token", "https://example.com", "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	err = client.UpdateComment(context.Background(), "", "updated")
	if err == nil {
		t.Fatal("UpdateComment() returned nil error for empty comment ID")
	}
	if !strings.Contains(err.Error(), "comment ID is required") {
		t.Fatalf("UpdateComment() error = %v, want comment ID is required", err)
	}
}

func TestUpdateCommentReturnsErrorOnUnsuccessfulMutation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"commentUpdate":{"success":false}}}`))
	}))
	defer srv.Close()

	client, err := NewLinearClient("token", srv.URL, "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	err = client.UpdateComment(context.Background(), "comment-1", "updated")
	if err == nil {
		t.Fatal("UpdateComment() returned nil error for unsuccessful mutation")
	}
	if !strings.Contains(err.Error(), "commentUpdate unsuccessful") {
		t.Fatalf("UpdateComment() error = %v, want commentUpdate unsuccessful", err)
	}
}

func TestAddLink(t *testing.T) {
	t.Parallel()

	var authHeader string
	var capturedBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		capturedBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"attachmentCreate":{"success":true}}}`))
	}))
	defer srv.Close()

	client, err := NewLinearClient("token", srv.URL, "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	if err := client.AddLink(context.Background(), "issue-1", "PR", "https://github.com/acme/repo/pull/new/test"); err != nil {
		t.Fatalf("AddLink() error = %v", err)
	}
	if authHeader != "token" {
		t.Fatalf("Authorization header = %q, want %q", authHeader, "token")
	}
	for _, want := range []string{
		"attachmentCreate",
		`"issueId":"issue-1"`,
		`"title":"PR"`,
		`"url":"https://github.com/acme/repo/pull/new/test"`,
	} {
		if !strings.Contains(capturedBody, want) {
			t.Fatalf("request body missing %q: %s", want, capturedBody)
		}
	}
}

func TestAddLinkRejectsMissingFields(t *testing.T) {
	t.Parallel()

	client, err := NewLinearClient("token", "https://example.com", "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	tests := []struct {
		name  string
		issue string
		title string
		url   string
		want  string
	}{
		{name: "missing issue id", title: "PR", url: "https://example.com", want: "issue ID is required"},
		{name: "missing title", issue: "issue-1", url: "https://example.com", want: "link title is required"},
		{name: "missing url", issue: "issue-1", title: "PR", want: "link url is required"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := client.AddLink(context.Background(), tc.issue, tc.title, tc.url)
			if err == nil {
				t.Fatal("AddLink() returned nil error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AddLink() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAddLinkReturnsErrorOnUnsuccessfulMutation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"attachmentCreate":{"success":false}}}`))
	}))
	defer srv.Close()

	client, err := NewLinearClient("token", srv.URL, "proj", []string{"Todo"}, "")
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	err = client.AddLink(context.Background(), "issue-1", "PR", "https://example.com")
	if err == nil {
		t.Fatal("AddLink() returned nil error for unsuccessful mutation")
	}
	if !strings.Contains(err.Error(), "attachmentCreate unsuccessful") {
		t.Fatalf("AddLink() error = %v, want attachmentCreate unsuccessful", err)
	}
}
