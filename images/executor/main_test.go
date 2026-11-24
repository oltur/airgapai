package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"airgapai/gateway/approval"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeForgejo serves the endpoints the executor uses, with teams that tests can change, and records
// every issue filed.
type fakeForgejo struct {
	mu      sync.Mutex
	members map[string]string // user to team
	issues  []map[string]string
	fail    int // a status for the create call to answer with instead of 201
	onIssue func() // called when an issue request arrives, before it is filed
}

func (f *fakeForgejo) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/orgs/maintenance/teams", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token test-token" {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"name":"Owners"},{"id":2,"name":"technicians"},{"id":3,"name":"engineers"}]`))
	})
	mux.HandleFunc("GET /api/v1/teams/{id}/members/{user}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		team := map[string]string{"2": "technicians", "3": "engineers"}[r.PathValue("id")]
		if team != "" && f.members[r.PathValue("user")] == team {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("POST /api/v1/repos/maintenance/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		if repo := r.PathValue("repo"); repo != "tickets" && repo != "tickets-restricted" {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		hook := f.onIssue
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail != 0 {
			http.Error(w, "", f.fail)
			return
		}
		var issue map[string]string
		if err := json.NewDecoder(r.Body).Decode(&issue); err != nil {
			t.Error(err)
		}
		issue["repo"] = r.PathValue("repo")
		f.issues = append(f.issues, issue)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]int{"number": len(f.issues)})
	})
	return mux
}

func (f *fakeForgejo) setTeam(user, team string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[user] = team
}

func (f *fakeForgejo) setFail(fail int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

func (f *fakeForgejo) issue(i int) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issues[i]
}

func (f *fakeForgejo) filed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.issues)
}

// A visibility with no repository files nothing, before any request reaches Forgejo.
func TestUnknownVisibilityFilesNothing(t *testing.T) {
	fake := &fakeForgejo{members: map[string]string{}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	fj := newForgejo(srv.URL, "test-token", "maintenance")
	for _, v := range []string{"", "secret", "Restricted"} {
		if _, _, err := fj.createIssue(context.Background(), v, "t", "b"); !errors.Is(err, errVisibility) {
			t.Errorf("visibility %q: %v", v, err)
		}
	}
	if n := fake.filed(); n != 0 {
		t.Errorf("%d issues filed", n)
	}
	// The known ones file where directory.TicketRepos says.
	for v, want := range map[string]string{approval.Public: "tickets", approval.Restricted: "tickets-restricted"} {
		_, repo, err := fj.createIssue(context.Background(), v, "t", "b")
		if err != nil || repo != "maintenance/"+want || fake.issue(fake.filed() - 1)["repo"] != want {
			t.Errorf("visibility %q: filed in %q, %v", v, repo, err)
		}
	}
}

func TestRoleFromTeams(t *testing.T) {
	fake := &fakeForgejo{members: map[string]string{"alice": "technicians", "bob": "engineers"}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	fj := newForgejo(srv.URL, "test-token", "maintenance")
	for user, want := range map[string]string{"alice": "technician", "bob": "engineer", "carol": "", "../admin": "", "a b": "", "Alice": ""} {
		if got, err := fj.Role(context.Background(), user); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", user, got, err, want)
		}
	}
	// A Forgejo that refuses the token is an error, not "no role".
	bad := newForgejo(srv.URL, "wrong", "maintenance")
	if _, err := bad.Role(context.Background(), "alice"); err == nil {
		t.Error("a refused token gave a role answer")
	}
}

// The HTTP path end to end, against the test database and a fake Forgejo. Needs GATEWAY_TEST_DSN and
// GATEWAY_ADMIN_DSN as the approval tests do; make test-db runs it.
func TestExecuteFilesOnlyConfirmedCalls(t *testing.T) {
	dsn, adminDSN := os.Getenv("GATEWAY_TEST_DSN"), os.Getenv("GATEWAY_ADMIN_DSN")
	if dsn == "" || adminDSN == "" {
		t.Skip("GATEWAY_TEST_DSN and GATEWAY_ADMIN_DSN are not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "TRUNCATE gateway.audit, gateway.confirmations, gateway.proposals, gateway.tool_permissions"); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	fake := &fakeForgejo{members: map[string]string{"alice": "technicians", "bob": "technicians"}}
	fsrv := httptest.NewServer(fake.handler(t))
	defer fsrv.Close()
	fj := newForgejo(fsrv.URL, "test-token", "maintenance")
	store := approval.New(pool, fj, []approval.Tool{approval.CreateTicket}, time.Now)
	if err := store.Grant(ctx, "cluster-admin", "technician", "create_ticket"); err != nil {
		t.Fatal(err)
	}
	s := &server{store: store, forgejo: fj}
	const args = `{"title":"Pump 3 leaks","body":"Seen at the seal.","priority":"high"}`
	confirmFrom := func(sources []approval.Source) string {
		p, err := store.Propose(ctx, "alice", "create_ticket", []byte(args), sources)
		if err != nil {
			t.Fatal(err)
		}
		token, err := store.Confirm(ctx, "alice", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	confirm := func() string { return confirmFrom(nil) }
	postBody := func(token, user, tool, a string) (int, map[string]string) {
		body, _ := json.Marshal(map[string]any{"token": token, "user": user, "tool": tool, "args": json.RawMessage(a)})
		w := httptest.NewRecorder()
		s.handleExecute(w, httptest.NewRequest(http.MethodPost, "/execute", bytes.NewReader(body)))
		var answer map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &answer)
		return w.Code, answer
	}
	post := func(token, user, tool, a string) int {
		code, _ := postBody(token, user, tool, a)
		return code
	}

	token := confirm()
	for name, code := range map[string]int{
		"no token":          post("", "alice", "create_ticket", args),
		"forged token":      post("forged", "alice", "create_ticket", args),
		"another user":      post(token, "bob", "create_ticket", args),
		"another tool":      post(token, "alice", "delete_repo", args),
		"altered arguments": post(token, "alice", "create_ticket", strings.Replace(args, "high", "low", 1)),
	} {
		if code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, code)
		}
	}
	if n := fake.filed(); n != 0 {
		t.Fatalf("%d issues filed by refused calls", n)
	}
	if code := post(token, "alice", "create_ticket", args); code != http.StatusOK {
		t.Fatalf("the confirmed call: %d", code)
	}
	if code := post(token, "alice", "create_ticket", args); code != http.StatusForbidden {
		t.Fatalf("the replay: %d", code)
	}
	if n := fake.filed(); n != 1 {
		t.Fatalf("%d issues filed, want 1", n)
	}
	issue := fake.issue(0)
	if issue["repo"] != "tickets" {
		t.Errorf("a proposal with no restricted source was filed in %q", issue["repo"])
	}
	if issue["title"] != "Pump 3 leaks" || !strings.HasPrefix(issue["body"], "Filed for alice after their confirmation of proposal ") ||
		!strings.HasSuffix(issue["body"], ". Priority: high.\n\n---\n\nSeen at the seal.") {
		t.Fatalf("the issue: %v", issue)
	}

	// Leaving the team in Forgejo after confirming stops the call.
	token = confirm()
	fake.setTeam("alice", "")
	if code := post(token, "alice", "create_ticket", args); code != http.StatusForbidden {
		t.Fatalf("after leaving the team: %d", code)
	}
	fake.setTeam("alice", "technicians")

	// If Forgejo rejects the call, nothing was filed for certain, and the caller is told so. The
	// confirmation is still used up, and the failure is logged.
	fake.setFail(http.StatusUnprocessableEntity)
	if code, body := postBody(token, "alice", "create_ticket", args); code != http.StatusBadGateway || body["failed"] != "Forgejo refused the ticket (422)" {
		t.Fatalf("with Forgejo rejecting the call: %d %v", code, body)
	}
	fake.setFail(0)
	if code := post(token, "alice", "create_ticket", args); code != http.StatusForbidden {
		t.Fatalf("a retry of a consumed token: %d", code)
	}
	// A 5xx may come after Forgejo created the issue, so the result is unknown, not "not filed".
	token = confirm()
	fake.setFail(http.StatusInternalServerError)
	if code, body := postBody(token, "alice", "create_ticket", args); code != http.StatusInternalServerError || body["failed"] != "" {
		t.Fatalf("with Forgejo failing: %d %v", code, body)
	}
	fake.setFail(0)
	if n := fake.filed(); n != 1 {
		t.Fatalf("%d issues filed, want 1", n)
	}
	// A caller that goes away after the token is consumed does not stop the call or its logging.
	token = confirm()
	reqCtx, cancelReq := context.WithCancel(ctx)
	fake.mu.Lock()
	fake.onIssue = cancelReq
	fake.mu.Unlock()
	number, repo, err := s.run(reqCtx, executeRequest{Token: token, User: "alice", Tool: "create_ticket", Args: json.RawMessage(args)})
	fake.mu.Lock()
	fake.onIssue = nil
	fake.mu.Unlock()
	if err != nil || number != 2 || repo != "maintenance/tickets" || reqCtx.Err() == nil {
		t.Fatalf("with the caller gone: issue %d, %v, context %v", number, err, reqCtx.Err())
	}

	// A proposal drawn on a restricted source is filed where only restricted readers are.
	token = confirmFrom([]approval.Source{{Path: "a.md", ContentHash: "h", Classification: approval.Public},
		{Path: "s.md", ContentHash: "h", Classification: approval.Restricted}})
	if code := post(token, "alice", "create_ticket", args); code != http.StatusOK {
		t.Fatalf("the restricted call: %d", code)
	}
	if repo := fake.issue(fake.filed() - 1)["repo"]; repo != "tickets-restricted" {
		t.Errorf("a restricted proposal was filed in %q", repo)
	}

	var outcomes string
	if err := admin.QueryRow(ctx, "SELECT string_agg(decision, ',' ORDER BY seq) FROM gateway.audit WHERE action = 'result'").Scan(&outcomes); err != nil || outcomes != "succeeded,failed,unknown,succeeded,succeeded" {
		t.Fatalf("result entries: %q, %v", outcomes, err)
	}
	if bad, _, err := store.VerifyAudit(ctx); err != nil || bad != 0 {
		t.Fatalf("audit chain: %d, %v", bad, err)
	}
}
