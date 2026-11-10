package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// forgejo is the executor's view of Forgejo: who is in which team, and filing issues. Its token is
// an owner's of the tickets organisation only, scoped to issues and to reading the organisation.
type forgejo struct {
	base, token, org, repo string
	client                 *http.Client
}

func newForgejo(base, token, org, repo string) *forgejo {
	return &forgejo{base: base, token: token, org: org, repo: repo, client: &http.Client{Timeout: 15 * time.Second}}
}

// Forgejo's user names. Anything else is refused before it reaches a URL.
var userName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

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

// Teams map Forgejo teams to reader roles, the higher first.
var teamRoles = []struct{ team, role string }{{"engineers", "engineer"}, {"technicians", "technician"}}

// Role asks Forgejo now, every time, so a change of team takes effect on the next decision.
func (f *forgejo) Role(ctx context.Context, user string) (string, error) {
	if !userName.MatchString(user) {
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
	for _, tr := range teamRoles {
		id, ok := ids[tr.team]
		if !ok {
			continue
		}
		status, _, err := f.do(ctx, http.MethodGet, fmt.Sprintf("/teams/%d/members/%s", id, url.PathEscape(user)), nil)
		if err != nil {
			return "", err
		}
		switch status {
		case http.StatusOK:
			return tr.role, nil
		case http.StatusNotFound:
		default:
			return "", fmt.Errorf("checking %s membership: %d", tr.team, status)
		}
	}
	return "", nil
}

// createIssue files the ticket and returns its number.
func (f *forgejo) createIssue(ctx context.Context, title, body string) (int64, error) {
	status, raw, err := f.do(ctx, http.MethodPost,
		"/repos/"+url.PathEscape(f.org)+"/"+url.PathEscape(f.repo)+"/issues", map[string]string{"title": title, "body": body})
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated {
		return 0, fmt.Errorf("creating the issue: %d", status)
	}
	var issue struct {
		Number int64 `json:"number"`
	}
	err = json.Unmarshal(raw, &issue)
	return issue.Number, err
}
