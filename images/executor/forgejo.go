package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"airgapai/gateway/approval"
	"airgapai/gateway/directory"
)

// forgejo is the executor's view of Forgejo: who is in which team, and filing issues. Its token is
// an owner's of the tickets organisation only, scoped to issues and to reading the organisation.
type forgejo struct {
	base, token, org string
	repos            map[string]string // by visibility: where a ticket of that visibility is filed
	client           *http.Client
}

// newForgejo files public tickets in repo, which every team reads, and restricted ones in restricted,
// which only the teams of restricted readers do. See ADR 0021.
func newForgejo(base, token, org, repo, restricted string) *forgejo {
	return &forgejo{base: base, token: token, org: org, client: &http.Client{Timeout: 15 * time.Second},
		repos: map[string]string{approval.Public: repo, approval.Restricted: restricted}}
}

// refusedError is Forgejo answering and refusing, as opposed to no answer: then the issue does not exist.
type refusedError struct{ status int }

func (e *refusedError) Error() string { return fmt.Sprintf("Forgejo refused the issue: %d", e.status) }

func (f *forgejo) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.base+"/api/v1"+path, r)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "token "+f.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, err
}

// Role asks Forgejo now, every time, so a change of team takes effect on the next decision.
func (f *forgejo) Role(ctx context.Context, user string) (string, error) {
	if !directory.UserName.MatchString(user) {
		return "", nil
	}
	status, raw, err := f.do(ctx, http.MethodGet, "/orgs/"+url.PathEscape(f.org)+"/teams", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("listing teams: %d", status)
	}
	var teams []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &teams); err != nil {
		return "", err
	}
	ids := map[string]int64{}
	for _, t := range teams {
		ids[t.Name] = t.ID
	}
	for _, tr := range directory.TeamRoles {
		id, ok := ids[tr.Team]
		if !ok {
			continue
		}
		status, _, err := f.do(ctx, http.MethodGet, fmt.Sprintf("/teams/%d/members/%s", id, url.PathEscape(user)), nil)
		if err != nil {
			return "", err
		}
		switch status {
		case http.StatusOK:
			return tr.Role, nil
		case http.StatusNotFound:
		default:
			return "", fmt.Errorf("checking %s membership: %d", tr.Team, status)
		}
	}
	return "", nil
}

// errVisibility is an execution whose visibility has no repository: nothing is filed.
var errVisibility = errors.New("no repository for this visibility")

// createIssue files the ticket in the repository for its visibility, and returns its number.
func (f *forgejo) createIssue(ctx context.Context, visibility, title, body string) (int64, error) {
	repo, ok := f.repos[visibility]
	if !ok {
		return 0, errVisibility
	}
	status, raw, err := f.do(ctx, http.MethodPost,
		"/repos/"+url.PathEscape(f.org)+"/"+url.PathEscape(repo)+"/issues", map[string]string{"title": title, "body": body})
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated {
		return 0, &refusedError{status}
	}
	var issue struct {
		Number int64 `json:"number"`
	}
	err = json.Unmarshal(raw, &issue)
	return issue.Number, err
}
