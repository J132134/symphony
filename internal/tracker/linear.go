// Package tracker implements the Linear GraphQL tracker client.
package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"symphony/internal/types"
)

const issuesQuery = `
query($projectSlug: String!, $states: [String!], $after: String) {
  issues(
    filter: {
      project: { slugId: { eq: $projectSlug } }
      state: { name: { in: $states } }
    }
    first: 50
    after: $after
    orderBy: createdAt
  ) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id identifier title description priority
      state { name }
      branchName url
      comments(last: 1, orderBy: updatedAt) {
        nodes { body createdAt updatedAt }
      }
      labels { nodes { name } }
      relations {
        nodes { type relatedIssue { id identifier state { name } } }
      }
      createdAt updatedAt
    }
  }
}`

const issuesByIDsQuery = `
query($ids: [ID!]!) {
  issues(filter: { id: { in: $ids } }) {
    nodes { id identifier state { name } }
  }
}`

const issueByIDQuery = `
query($id: String!) {
  issue(id: $id) {
    id identifier title description priority
    state { name }
    branchName url
    comments(first: 1) {
      nodes { body createdAt updatedAt }
    }
    labels { nodes { name } }
    relations {
      nodes { type relatedIssue { id identifier state { name } } }
    }
    createdAt updatedAt
  }
}`

const commentCreateMutation = `
mutation($input: CommentCreateInput!) {
  commentCreate(input: $input) {
    success
  }
}`

const attachmentCreateMutation = `
mutation($input: AttachmentCreateInput!) {
  attachmentCreate(input: $input) {
    success
  }
}`

const issueStateLookupQuery = `
query($id: String!) {
  issue(id: $id) {
    team {
      states {
        nodes { id name }
      }
    }
  }
}`

const issueUpdateMutation = `
mutation($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) {
    success
  }
}`

const projectPingQuery = `
query($projectSlug: String!) {
  projects(filter: { slugId: { eq: $projectSlug } }, first: 1) {
    nodes { id }
  }
}`

const viewerQuery = `query { viewer { id } }`

const issuesQueryWithAssignee = `
query($projectSlug: String!, $states: [String!], $after: String, $assigneeId: String!) {
  issues(
    filter: {
      project: { slugId: { eq: $projectSlug } }
      state: { name: { in: $states } }
      assignee: { id: { eq: $assigneeId } }
    }
    first: 50
    after: $after
    orderBy: createdAt
  ) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id identifier title description priority
      state { name }
      branchName url
      comments(last: 1, orderBy: updatedAt) {
        nodes { body createdAt updatedAt }
      }
      labels { nodes { name } }
      relations {
        nodes { type relatedIssue { id identifier state { name } } }
      }
      createdAt updatedAt
    }
  }
}`

// LinearClient fetches issues from the Linear GraphQL API.
type LinearClient struct {
	endpoint     string
	projectSlug  string
	activeStates []string
	assigneeID   string
	client       *http.Client
}

func NewLinearClient(apiKey, endpoint, projectSlug string, activeStates []string, assignee string) (*LinearClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("Linear API key is required")
	}
	if projectSlug == "" {
		return nil, fmt.Errorf("Linear project slug is required")
	}
	c := &LinearClient{
		endpoint:     endpoint,
		projectSlug:  projectSlug,
		activeStates: activeStates,
		client: &http.Client{
			Transport: &authTransport{key: apiKey, base: http.DefaultTransport},
			Timeout:   30 * time.Second,
		},
	}
	if assignee == "me" {
		id, err := c.fetchViewerID(context.Background())
		if err != nil {
			return nil, fmt.Errorf("fetch viewer id: %w", err)
		}
		c.assigneeID = id
	} else {
		c.assigneeID = assignee
	}
	return c, nil
}

func (c *LinearClient) fetchViewerID(ctx context.Context) (string, error) {
	data, err := c.execute(ctx, viewerQuery, nil)
	if err != nil {
		return "", err
	}
	viewer, _ := data["viewer"].(map[string]any)
	id := strVal(viewer["id"])
	if id == "" {
		return "", fmt.Errorf("viewer query returned empty id")
	}
	return id, nil
}

// ExecuteGraphQL executes an arbitrary GraphQL query or mutation against the Linear API.
func (c *LinearClient) ExecuteGraphQL(ctx context.Context, query string, variables map[string]any) (map[string]any, error) {
	return c.execute(ctx, query, variables)
}

type authTransport struct {
	key  string
	base http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.Header.Set("Authorization", t.key)
	req2.Header.Set("Content-Type", "application/json")
	return t.base.RoundTrip(req2)
}

func (c *LinearClient) FetchCandidateIssues(ctx context.Context) ([]*types.Issue, error) {
	return c.fetchPaginated(ctx, c.activeStates)
}

func (c *LinearClient) FetchIssueByID(ctx context.Context, id string) (*types.Issue, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("issue ID is required")
	}
	data, err := c.execute(ctx, issueByIDQuery, map[string]any{"id": id})
	if err != nil {
		return nil, err
	}
	node, ok := data["issue"].(map[string]any)
	if !ok || strVal(node["id"]) == "" {
		return nil, nil
	}
	return normalizeIssue(node), nil
}

func (c *LinearClient) FetchIssuesByStates(ctx context.Context, states []string) ([]*types.Issue, error) {
	if len(states) == 0 {
		return nil, nil
	}
	return c.fetchPaginated(ctx, states)
}

func (c *LinearClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	data, err := c.execute(ctx, issuesByIDsQuery, map[string]any{"ids": ids})
	if err != nil {
		return nil, err
	}
	issuesData, _ := data["issues"].(map[string]any)
	nodes, _ := issuesData["nodes"].([]any)

	var result []*types.Issue
	for _, n := range nodes {
		node, ok := n.(map[string]any)
		if !ok {
			continue
		}
		stateMap, _ := node["state"].(map[string]any)
		result = append(result, &types.Issue{
			ID:         strVal(node["id"]),
			Identifier: strVal(node["identifier"]),
			State:      strVal(stateMap["name"]),
		})
	}
	return result, nil
}

func (c *LinearClient) AddComment(ctx context.Context, issueID, body string) error {
	if strings.TrimSpace(issueID) == "" {
		return fmt.Errorf("issue ID is required")
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("comment body is required")
	}
	data, err := c.execute(ctx, commentCreateMutation, map[string]any{
		"input": map[string]any{
			"issueId": issueID,
			"body":    body,
		},
	})
	if err != nil {
		return err
	}
	payload, _ := data["commentCreate"].(map[string]any)
	if ok, _ := payload["success"].(bool); !ok {
		return fmt.Errorf("commentCreate unsuccessful")
	}
	return nil
}

func (c *LinearClient) AddLink(ctx context.Context, issueID, title, targetURL string) error {
	if strings.TrimSpace(issueID) == "" {
		return fmt.Errorf("issue ID is required")
	}
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("link title is required")
	}
	if strings.TrimSpace(targetURL) == "" {
		return fmt.Errorf("link url is required")
	}
	data, err := c.execute(ctx, attachmentCreateMutation, map[string]any{
		"input": map[string]any{
			"issueId": issueID,
			"title":   title,
			"url":     targetURL,
		},
	})
	if err != nil {
		return err
	}
	payload, _ := data["attachmentCreate"].(map[string]any)
	if ok, _ := payload["success"].(bool); !ok {
		return fmt.Errorf("attachmentCreate unsuccessful")
	}
	return nil
}

func (c *LinearClient) UpdateIssueState(ctx context.Context, issueID, stateName string) error {
	if strings.TrimSpace(issueID) == "" {
		return fmt.Errorf("issue id is required")
	}
	stateName = strings.TrimSpace(stateName)
	if stateName == "" {
		return nil
	}

	data, err := c.execute(ctx, issueStateLookupQuery, map[string]any{"id": issueID})
	if err != nil {
		return err
	}
	issueData, _ := data["issue"].(map[string]any)
	teamData, _ := issueData["team"].(map[string]any)
	statesData, _ := teamData["states"].(map[string]any)

	var stateID string
	for _, raw := range castSlice(statesData["nodes"]) {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.EqualFold(strVal(node["name"]), stateName) {
			stateID = strVal(node["id"])
			break
		}
	}
	if stateID == "" {
		return fmt.Errorf("state %q not found", stateName)
	}

	data, err = c.execute(ctx, issueUpdateMutation, map[string]any{
		"id":      issueID,
		"stateId": stateID,
	})
	if err != nil {
		return err
	}
	payload, _ := data["issueUpdate"].(map[string]any)
	if ok, _ := payload["success"].(bool); !ok {
		return fmt.Errorf("issueUpdate returned success=false")
	}
	return nil
}

func (c *LinearClient) Ping(ctx context.Context) error {
	data, err := c.execute(ctx, projectPingQuery, map[string]any{"projectSlug": c.projectSlug})
	if err != nil {
		return err
	}
	projectsData, _ := data["projects"].(map[string]any)
	nodes, _ := projectsData["nodes"].([]any)
	if len(nodes) == 0 {
		return fmt.Errorf("project %q not found", c.projectSlug)
	}
	return nil
}

func (c *LinearClient) fetchPaginated(ctx context.Context, states []string) ([]*types.Issue, error) {
	var all []*types.Issue
	var cursor string

	query := issuesQuery
	if c.assigneeID != "" {
		query = issuesQueryWithAssignee
	}

	for {
		vars := map[string]any{"projectSlug": c.projectSlug, "states": states}
		if cursor != "" {
			vars["after"] = cursor
		}
		if c.assigneeID != "" {
			vars["assigneeId"] = c.assigneeID
		}

		data, err := c.execute(ctx, query, vars)
		if err != nil {
			return nil, err
		}

		issuesData, ok := data["issues"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unexpected response shape")
		}

		nodes, _ := issuesData["nodes"].([]any)
		for _, n := range nodes {
			if node, ok := n.(map[string]any); ok {
				all = append(all, normalizeIssue(node))
			}
		}

		pageInfo, _ := issuesData["pageInfo"].(map[string]any)
		if hasNext, _ := pageInfo["hasNextPage"].(bool); !hasNext {
			break
		}
		cursor, ok = pageInfo["endCursor"].(string)
		if !ok || cursor == "" {
			return nil, fmt.Errorf("pagination: hasNextPage=true but endCursor missing")
		}
	}
	return all, nil
}

func (c *LinearClient) execute(ctx context.Context, query string, vars map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Linear API status %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	var result struct {
		Data   map[string]any `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("json decode: %w", err)
	}
	if len(result.Errors) > 0 {
		msgs := make([]string, len(result.Errors))
		for i, e := range result.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("GraphQL errors: %s", strings.Join(msgs, "; "))
	}
	if result.Data == nil {
		return nil, fmt.Errorf("response missing 'data'")
	}
	return result.Data, nil
}

func normalizeIssue(node map[string]any) *types.Issue {
	stateMap, _ := node["state"].(map[string]any)

	var labels []string
	if ld, ok := node["labels"].(map[string]any); ok {
		for _, n := range castSlice(ld["nodes"]) {
			if nm, ok := n.(map[string]any); ok {
				if name := strVal(nm["name"]); name != "" {
					labels = append(labels, strings.ToLower(name))
				}
			}
		}
	}

	var blockedBy []types.BlockerRef
	if rd, ok := node["relations"].(map[string]any); ok {
		for _, n := range castSlice(rd["nodes"]) {
			nm, ok := n.(map[string]any)
			if !ok || strVal(nm["type"]) != "blocked_by" {
				continue
			}
			related, ok := nm["relatedIssue"].(map[string]any)
			if !ok || strVal(related["id"]) == "" {
				continue
			}
			relState, _ := related["state"].(map[string]any)
			blockedBy = append(blockedBy, types.BlockerRef{
				ID:         strVal(related["id"]),
				Identifier: strVal(related["identifier"]),
				State:      strVal(relState["name"]),
			})
		}
	}

	iss := &types.Issue{
		ID:          strVal(node["id"]),
		Identifier:  strVal(node["identifier"]),
		Title:       strVal(node["title"]),
		Description: strVal(node["description"]),
		State:       strVal(stateMap["name"]),
		BranchName:  strVal(node["branchName"]),
		URL:         strVal(node["url"]),
		Labels:      labels,
		BlockedBy:   blockedBy,
	}
	if pri, ok := node["priority"].(float64); ok && pri > 0 {
		p := int(pri)
		iss.Priority = &p
	}
	iss.CreatedAt = parseISO(strVal(node["createdAt"]))
	iss.UpdatedAt = parseISO(strVal(node["updatedAt"]))
	iss.LastComment = latestComment(node["comments"])
	return iss
}

func latestComment(raw any) *types.Comment {
	comments, _ := raw.(map[string]any)
	nodes := castSlice(comments["nodes"])
	var latest *types.Comment
	var latestAt time.Time

	for _, rawNode := range nodes {
		node, ok := rawNode.(map[string]any)
		if !ok {
			continue
		}
		comment := &types.Comment{
			Body:      strVal(node["body"]),
			CreatedAt: parseISO(strVal(node["createdAt"])),
			UpdatedAt: parseISO(strVal(node["updatedAt"])),
		}
		if strings.TrimSpace(comment.Body) == "" {
			continue
		}
		commentAt := latestCommentTime(comment)
		if latest == nil || commentAt.After(latestAt) {
			latest = comment
			latestAt = commentAt
		}
	}

	return latest
}

func latestCommentTime(comment *types.Comment) time.Time {
	if comment == nil {
		return time.Time{}
	}
	if comment.UpdatedAt != nil {
		return *comment.UpdatedAt
	}
	if comment.CreatedAt != nil {
		return *comment.CreatedAt
	}
	return time.Time{}
}

func parseISO(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, strings.Replace(s, "Z", "+00:00", 1))
	if err != nil {
		return nil
	}
	return &t
}

func strVal(v any) string { s, _ := v.(string); return s }
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
func castSlice(v any) []any { s, _ := v.([]any); return s }
