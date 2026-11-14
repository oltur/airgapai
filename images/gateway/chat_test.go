package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"airgapai/gateway/approval"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The chat loop has no path to a confirmation or to the executor: it can search and propose only.
func TestChatCannotConfirm(t *testing.T) {
	src, err := os.ReadFile("chat.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"Confirm(", "executor", "Execute("} {
		if strings.Contains(string(src), banned) {
			t.Errorf("chat.go mentions %q", banned)
		}
	}
}

func chatDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	t.Cleanup(pool.Close)
	return pool
}

var proposalLink = regexp.MustCompile(`/proposals/([0-9a-f-]{36})`)

// A whole turn against the test database, a scripted model and a fake retrieval service.
func TestChatTurn(t *testing.T) {
	pool := chatDatabase(t)
	h := newHarness(t, pool)
	ctx := context.Background()
	if err := h.store.Grant(ctx, "cluster-admin", "technician", "create_ticket"); err != nil {
		t.Fatal(err)
	}
	alice := h.browser()
	if got := alice.get("/chat"); got.status != http.StatusSeeOther || got.location != "/login?next=%2Fchat" {
		t.Fatalf("a visitor should be sent to sign in: %+v", got)
	}
	if got := alice.signIn("/chat", "alice", "maintenance:technicians"); got.location != "/chat" {
		t.Fatalf("after sign-in, back to the chat: %+v", got)
	}
	form := alice.get("/chat")
	csrfToken := csrf(t, form.body)
	ask := func(b *browser, question, token string) answer {
		return b.post("/chat", url.Values{"csrf": {token}, "question": {question}}, nil)
	}

	h.model.script(
		call("search_manuals", `{"query":"pump seal","role":"engineer"}`), // tries to choose its own role
		call("search_manuals", `{"query":"pump seal"}`),
		call("create_ticket", `{"title":"Pump 3 seal leaks","body":"Seen at the seal.","priority":"high"}`),
		reply("The seal leaks [1]. I proposed a ticket. <script>alert(1)</script>"),
	)
	got := ask(alice, "Pump 3 leaks at the seal. File a ticket.", csrfToken)
	if got.status != http.StatusOK {
		t.Fatalf("the turn: %d %s", got.status, got.body)
	}
	for _, want := range []string{"A tool call was refused", "unknown field", "Searched the manuals for “pump seal”: 2 passages",
		"A ticket was proposed, not filed", "answer · written by the model", "&lt;script&gt;alert(1)&lt;/script&gt;", "pumps.md, chunk 1, public"} {
		if !strings.Contains(got.body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(got.body, "<script>") {
		t.Error("the model's markup reached the page")
	}
	if roles := h.search.asked(); len(roles) != 1 || roles[0] != "technician" {
		t.Errorf("retrieval was asked for roles %v, want only the session's technician", roles)
	}
	m := proposalLink.FindStringSubmatch(got.body)
	if m == nil {
		t.Fatal("no proposal link on the page")
	}

	// What the model saw: the passages, numbered, and the proposal's ID with its status, but never
	// the session's CSRF token.
	reqs := h.model.seen()
	if len(reqs) != 4 {
		t.Fatalf("model calls: %d", len(reqs))
	}
	all, _ := json.Marshal(reqs)
	for _, want := range []string{`\"source\":1`, "Check the seal for leaks.", m[1], "it is not filed"} {
		if !strings.Contains(string(all), want) {
			t.Errorf("the model was not sent %q", want)
		}
	}
	if strings.Contains(string(all), csrfToken) || strings.Contains(string(all), "Restricted procedure") {
		t.Error("the model was sent the CSRF token or restricted text")
	}

	// The proposal is alice's, with the passages the gateway found as its sources, not the model's
	// word for them.
	p, err := h.store.Proposal(ctx, "alice", uuid.MustParse(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	want := []approval.Source{{Path: "pumps.md", ChunkIndex: 0, ContentHash: "h0"}, {Path: "pumps.md", ChunkIndex: 1, ContentHash: "h1"}}
	if len(p.Sources) != 2 || p.Sources[0] != want[0] || p.Sources[1] != want[1] {
		t.Errorf("the proposal's sources: %+v", p.Sources)
	}
	// The confirmation page shows those passages' text, read with alice's role and escaped.
	pg := alice.get("/proposals/" + m[1])
	for _, want := range []string{"Check the seal for leaks.", "Replace the seal &lt;b&gt;yearly&lt;/b&gt;.", "SHA-256 h1, public"} {
		if !strings.Contains(pg.body, want) {
			t.Errorf("the confirmation page lacks %q", want)
		}
	}
	if n := len(h.executor.calls()); n != 0 {
		t.Errorf("a chat turn reached the executor %d times", n)
	}

	// The model is stopped after maxSteps calls that only call tools.
	h.model.script(call("search_manuals", `{"query":"a"}`), call("search_manuals", `{"query":"b"}`),
		call("search_manuals", `{"query":"c"}`), call("search_manuals", `{"query":"d"}`), reply("never reached"))
	if got := ask(alice, "Loop.", csrfToken); !strings.Contains(got.body, "was stopped") || len(h.model.seen()) != maxSteps {
		t.Errorf("the step limit: %d model calls", len(h.model.seen()))
	}
	// An unknown tool, and an argument the tool does not take, are refused and reported to the model.
	h.model.script(call("delete_repository", `{"name":"tickets"}`), call("create_ticket", `{"title":"t","body":"b","priority":"high","assignee":"root"}`), reply("done"))
	got = ask(alice, "Clean up.", csrfToken)
	if strings.Count(got.body, "A tool call was refused") != 2 || proposalLink.MatchString(got.body) {
		t.Errorf("unknown tool and extra argument: %s", got.body)
	}
	// A model that does not answer is reported as such.
	h.model.script()
	if got := ask(alice, "Anyone there?", csrfToken); got.status != http.StatusBadGateway {
		t.Errorf("a failed model: %d", got.status)
	}
	// Forms need the session's token, and questions a length.
	if got := ask(alice, "No token.", ""); got.status != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", got.status)
	}
	if got := ask(alice, strings.Repeat("x", maxQuestion+1), csrfToken); got.status != http.StatusBadRequest {
		t.Errorf("a long question: %d", got.status)
	}

	// Bob, an engineer without the grant: his searches use his role, and his proposal is refused.
	bob := h.browser()
	bob.signIn("/chat", "bob", "maintenance:engineers")
	before := len(h.search.asked())
	h.model.script(call("search_manuals", `{"query":"seal"}`), call("create_ticket", `{"title":"t","body":"b","priority":"low"}`), reply("ok"))
	got = ask(bob, "File it.", csrf(t, bob.get("/chat").body))
	if roles := h.search.asked()[before:]; len(roles) != 1 || roles[0] != "engineer" {
		t.Errorf("bob's search used roles %v", roles)
	}
	if !strings.Contains(got.body, "the user&#39;s role may not use this tool") || proposalLink.MatchString(got.body) {
		t.Errorf("bob's proposal: %s", got.body)
	}
}

// A source the viewer's role cannot read is listed without its text.
func TestSourceTextFollowsTheViewersRole(t *testing.T) {
	pool := chatDatabase(t)
	h := newHarness(t, pool)
	ctx := context.Background()
	if err := h.store.Grant(ctx, "cluster-admin", "technician", "create_ticket"); err != nil {
		t.Fatal(err)
	}
	pctx := withSession(ctx, session{identity: identity{user: "alice", role: "technician"}})
	p, err := h.store.Propose(pctx, "alice", "create_ticket", []byte(`{"title":"t","body":"b","priority":"low"}`),
		[]approval.Source{{Path: "pumps.md", ChunkIndex: 0, ContentHash: "h0"}, {Path: "secrets.md", ChunkIndex: 0, ContentHash: "h2"},
			{Path: "pumps.md", ChunkIndex: 1, ContentHash: "stale"}})
	if err != nil {
		t.Fatal(err)
	}
	alice := h.browser()
	alice.signIn("/", "alice", "maintenance:technicians")
	pg := alice.get("/proposals/" + p.ID.String())
	if !strings.Contains(pg.body, "Check the seal for leaks.") || strings.Contains(pg.body, "Restricted procedure") ||
		strings.Count(pg.body, "Not readable with your role, or changed since.") != 2 {
		t.Errorf("the sources as a technician sees them: %s", pg.body)
	}
	if roles := h.search.asked(); len(roles) != 1 || roles[0] != "technician" {
		t.Errorf("the source text was read with roles %v", roles)
	}
}
