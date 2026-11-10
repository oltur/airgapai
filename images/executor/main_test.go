package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	fail    bool
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
	mux.HandleFunc("POST /api/v1/repos/maintenance/tickets/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		var issue map[string]string
		if err := json.NewDecoder(r.Body).Decode(&issue); err != nil {
			t.Error(err)
		}
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

func (f *fakeForgejo) filed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.issues)
}

func TestRoleFromTeams(t *testing.T) {
	fake := &fakeForgejo{members: map[string]string{"alice": "technicians", "bob": "engineers"}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	fj := newForgejo(srv.URL, "test-token", "maintenance", "tickets")
	for user, want := range map[string]string{"alice": "technician", "bob": "engineer", "carol": "", "../admin": "", "a b": ""} {
		if got, err := fj.Role(context.Background(), user); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", user, got, err, want)
		}
	}
	// A Forgejo that refuses the token is an error, not "no role".
	bad := newForgejo(srv.URL, "wrong", "maintenance", "tickets")
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
	fj := newForgejo(fsrv.URL, "test-token", "maintenance", "tickets")
	store := approval.New(pool, fj, []approval.Tool{approval.CreateTicket}, time.Now)
	if err := store.Grant(ctx, "cluster-admin", "technician", "create_ticket"); err != nil {
		t.Fatal(err)
	}
	s := &server{store: store, forgejo: fj}
	const args = `{"title":"Pump 3 leaks","body":"Seen at the seal.","priority":"high"}`
	confirm := func() string {
		p, err := store.Propose(ctx, "alice", "create_ticket", []byte(args), nil)
		if err != nil {
			t.Fatal(err)
		}
		token, err := store.Confirm(ctx, "alice", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	post := func(token, user, tool, a string) int {
		body, _ := json.Marshal(map[string]any{"token": token, "user": user, "tool": tool, "args": json.RawMessage(a)})
		w := httptest.NewRecorder()
		s.handleExecute(w, httptest.NewRequest(http.MethodPost, "/execute", bytes.NewReader(body)))
		return w.Code
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
	issue := fake.issues[0]
	if issue["title"] != "Pump 3 leaks" || !strings.HasPrefix(issue["body"], "Seen at the seal.\n\n---\nPriority: high\nFiled for alice") {
		t.Fatalf("the issue: %v", issue)
	}

	// Leaving the team in Forgejo after confirming stops the call.
	token = confirm()
	fake.setTeam("alice", "")
	if code := post(token, "alice", "create_ticket", args); code != http.StatusForbidden {
		t.Fatalf("after leaving the team: %d", code)
	}
	fake.setTeam("alice", "technicians")

	// If Forgejo fails, the confirmation is still used up and the failure is logged.
	fake.fail = true
	if code := post(token, "alice", "create_ticket", args); code != http.StatusInternalServerError {
		t.Fatalf("with Forgejo failing: %d", code)
	}
	fake.fail = false
	if code := post(token, "alice", "create_ticket", args); code != http.StatusForbidden {
		t.Fatalf("a retry of a consumed token: %d", code)
	}
	if n := fake.filed(); n != 1 {
		t.Fatalf("%d issues filed, want 1", n)
	}
	var outcomes string
	if err := admin.QueryRow(ctx, "SELECT string_agg(decision, ',' ORDER BY seq) FROM gateway.audit WHERE action = 'result'").Scan(&outcomes); err != nil || outcomes != "succeeded,failed" {
		t.Fatalf("result entries: %q, %v", outcomes, err)
	}
	if bad, _, err := store.VerifyAudit(ctx); err != nil || bad != 0 {
		t.Fatalf("audit chain: %d, %v", bad, err)
	}
}
