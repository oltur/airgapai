package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"airgapai/gateway/approval"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// clock is a test clock that can be moved forward.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// fakeForgejo is Forgejo's OAuth side: it issues codes for test users and exchanges them for ID
// tokens, checking the client secret, the redirect and PKCE as Forgejo does. tamper changes the claims
// of the next token.
type fakeForgejo struct {
	mu     sync.Mutex
	url    string
	codes  map[string]grant
	tamper func(claims map[string]any)
}

type grant struct {
	user, nonce, challenge, redirect string
	groups                           []string
}

func (f *fakeForgejo) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		g, ok := f.codes[r.PostFormValue("code")]
		delete(f.codes, r.PostFormValue("code"))
		if !ok || r.PostFormValue("client_id") != "client" || r.PostFormValue("client_secret") != "secret" ||
			r.PostFormValue("redirect_uri") != g.redirect || challenge(r.PostFormValue("code_verifier")) != g.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		claims := map[string]any{"iss": f.url, "aud": []string{"client"}, "exp": time.Now().Add(8 * time.Hour).Unix(),
			"nonce": g.nonce, "preferred_username": g.user, "groups": g.groups}
		if f.tamper != nil {
			f.tamper(claims)
			f.tamper = nil
		}
		payload, _ := json.Marshal(claims)
		// Unsigned, which is what the gateway accepts from the token endpoint; see exchange.
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
		token := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "not-kept", "id_token": token, "token_type": "bearer"})
	})
	return mux
}

// signIn plays the user signing in at Forgejo: it reads the authorize redirect and issues a code.
func (f *fakeForgejo) signIn(t *testing.T, authorize, user string, groups []string) (code, state string) {
	t.Helper()
	u, err := url.Parse(authorize)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/login/oauth/authorize" || q.Get("client_id") != "client" || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" ||
		!strings.Contains(q.Get("scope"), "groups") {
		t.Fatalf("unexpected authorize request: %s", authorize)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	code = random()
	f.codes[code] = grant{user: user, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		redirect: q.Get("redirect_uri"), groups: groups}
	return code, q.Get("state")
}

func (f *fakeForgejo) setTamper(fn func(map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tamper = fn
}

// fakeExecutor records what the gateway sends and answers as configured.
type fakeExecutor struct {
	mu       sync.Mutex
	requests []map[string]any
	status   int
	answer   string
}

func (e *fakeExecutor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	e.requests = append(e.requests, req)
	w.WriteHeader(e.status)
	_, _ = io.WriteString(w, e.answer)
}

func (e *fakeExecutor) set(status int, answer string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status, e.answer = status, answer
}

func (e *fakeExecutor) calls() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.requests...)
}

type harness struct {
	t        *testing.T
	gateway  *httptest.Server
	forgejo  *fakeForgejo
	executor *fakeExecutor
	clock    *clock
	store    *approval.Store
}

func newHarness(t *testing.T, pool *pgxpool.Pool) *harness {
	h := &harness{t: t, forgejo: &fakeForgejo{codes: map[string]grant{}}, executor: &fakeExecutor{status: 200, answer: `{"issue":7}`}, clock: &clock{}}
	fsrv := httptest.NewServer(h.forgejo.handler(t))
	t.Cleanup(fsrv.Close)
	h.forgejo.url = fsrv.URL
	esrv := httptest.NewServer(h.executor)
	t.Cleanup(esrv.Close)
	// The gateway's own URL is known only once it listens, so the handler is set after.
	var s *server
	h.gateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.routes().ServeHTTP(w, r) }))
	t.Cleanup(h.gateway.Close)
	env := map[string]string{"GATEWAY_URL": h.gateway.URL, "FORGEJO_URL": fsrv.URL, "FORGEJO_BROWSER_URL": fsrv.URL,
		"EXECUTOR_URL": esrv.URL, "OAUTH_CLIENT_ID": "client", "OAUTH_CLIENT_SECRET": "secret"}
	var err error
	s, err = newServer(pool, func(k string) string { return env[k] }, h.clock.now)
	if err != nil {
		t.Fatal(err)
	}
	h.store = s.store
	return h
}

// browser is one user's browser: its own cookies, and redirects shown rather than followed.
type browser struct {
	h      *harness
	client *http.Client
}

func (h *harness) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{h: h, client: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type answer struct {
	status   int
	location string
	body     string
}

func (b *browser) do(req *http.Request) answer {
	b.h.t.Helper()
	resp, err := b.client.Do(req)
	if err != nil {
		b.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, resp.Header.Get("Location"), string(raw)}
}

func (b *browser) get(path string) answer {
	req, _ := http.NewRequest(http.MethodGet, b.h.gateway.URL+path, nil)
	return b.do(req)
}

// post submits a form as a browser on this page would, unless headers say otherwise.
func (b *browser) post(path string, form url.Values, headers map[string]string) answer {
	req, _ := http.NewRequest(http.MethodPost, b.h.gateway.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.h.gateway.URL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return b.do(req)
}

// signIn runs the whole sign-in and returns where the gateway sent the browser afterwards.
func (b *browser) signIn(next, user string, groups ...string) answer {
	b.h.t.Helper()
	start := b.get("/login?" + url.Values{"next": {next}}.Encode())
	if start.status != http.StatusSeeOther {
		b.h.t.Fatalf("login: %d", start.status)
	}
	code, state := b.h.forgejo.signIn(b.h.t, start.location, user, groups)
	return b.get("/callback?" + url.Values{"code": {code}, "state": {state}}.Encode())
}

// csrf reads the CSRF token from a page's form.
func csrf(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `name="csrf" value="`)
	if !ok {
		t.Fatalf("no CSRF token in the page")
	}
	token, _, _ := strings.Cut(rest, `"`)
	return token
}

func TestSignInRefusals(t *testing.T) {
	h := newHarness(t, nil)
	b := h.browser()
	if got := b.signIn("/", "alice", "maintenance:technicians"); got.status != http.StatusSeeOther || got.location != "/" {
		t.Fatalf("a good sign-in: %+v", got)
	}
	if home := b.get("/"); !strings.Contains(home.body, "alice (technician)") {
		t.Errorf("the start page does not show the user and role: %s", home.body)
	}
	if got := h.browser().signIn("/", "carol", "maintenance", "other:engineers"); got.status != http.StatusSeeOther {
		t.Errorf("a user with no team should still sign in: %d", got.status)
	}

	// Each claim the gateway checks, broken in turn.
	for name, tamper := range map[string]func(map[string]any){
		"another issuer":          func(c map[string]any) { c["iss"] = "http://elsewhere" },
		"another client":          func(c map[string]any) { c["aud"] = "other" },
		"expired":                 func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"another sign-in's nonce": func(c map[string]any) { c["nonce"] = "replayed" },
		"an upper-case name":      func(c map[string]any) { c["preferred_username"] = "Alice" },
		"a name with a slash":     func(c map[string]any) { c["preferred_username"] = "alice/../bob" },
	} {
		h.forgejo.setTamper(tamper)
		b := h.browser()
		if got := b.signIn("/", "alice", "maintenance:technicians"); got.status != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, got.status)
		}
		if home := b.get("/"); !strings.Contains(home.body, "Sign in with Forgejo") {
			t.Errorf("%s: a session was created", name)
		}
	}

	// A callback this browser did not start, a state used twice, and a code exchanged twice.
	b = h.browser()
	start := b.get("/login")
	code, state := h.forgejo.signIn(t, start.location, "alice", nil)
	other := h.browser()
	if got := other.get("/callback?" + url.Values{"code": {code}, "state": {state}}.Encode()); got.status != http.StatusBadRequest {
		t.Errorf("a callback without this browser's sign-in cookie: %d", got.status)
	}
	if got := b.get("/callback?" + url.Values{"code": {code}, "state": {state}}.Encode()); got.status != http.StatusSeeOther {
		t.Fatalf("the genuine callback: %d", got.status)
	}
	b2 := h.browser()
	start = b2.get("/login")
	_, state2 := h.forgejo.signIn(t, start.location, "alice", nil)
	if got := b2.get("/callback?" + url.Values{"code": {code}, "state": {state2}}.Encode()); got.status != http.StatusForbidden {
		t.Errorf("a used code: %d", got.status)
	}
	// The callback clears the sign-in cookie, so send it again by hand: the state itself must be spent.
	req, _ := http.NewRequest(http.MethodGet, h.gateway.URL+"/callback?"+url.Values{"code": {code}, "state": {state2}}.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: loginCookie, Value: state2})
	if got := h.browser().do(req); got.status != http.StatusBadRequest || !strings.Contains(got.body, "already used") {
		t.Errorf("a used state: %d", got.status)
	}

	// A sign-in returns only to this site's own pages.
	for _, next := range []string{"//evil.example/", "https://evil.example/", "/proposals/../../x", "/logout"} {
		if got := h.browser().signIn(next, "alice"); got.location != "/" {
			t.Errorf("next=%q sent the browser to %q", next, got.location)
		}
	}

	// A session ends after an hour, however long the ID token lasts.
	h.clock.advance(sessionTTL)
	if home := b.get("/"); !strings.Contains(home.body, "Sign in with Forgejo") {
		t.Error("a session outlived its hour")
	}
}

func TestPageHeaders(t *testing.T) {
	h := newHarness(t, nil)
	resp, err := http.Get(h.gateway.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "form-action 'self'") {
		t.Errorf("Content-Security-Policy: %q", csp)
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("missing Referrer-Policy or Cache-Control")
	}
}

func TestVisible(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\n":   "plain\ttext\n",
		"evil‮gnp.exe":    "evil⟨U+202E⟩gnp.exe",
		"zero​width":      "zero⟨U+200B⟩width",
		"bell\a":          "bell⟨U+0007⟩",
		"Grüße, ключ, 漢字": "Grüße, ключ, 漢字",
	} {
		if got := visible(in); got != want {
			t.Errorf("visible(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionRolesNeedTheSession(t *testing.T) {
	if _, err := (sessionRoles{}).Role(context.Background(), "alice"); err == nil {
		t.Error("a role without a session")
	}
	ctx := withSession(context.Background(), session{identity: identity{user: "alice", role: "technician"}})
	if _, err := (sessionRoles{}).Role(ctx, "bob"); err == nil {
		t.Error("alice's session gave bob a role")
	}
	if role, err := (sessionRoles{}).Role(ctx, "alice"); err != nil || role != "technician" {
		t.Errorf("got %q, %v", role, err)
	}
}

// The confirmation page end to end, against the test database, a fake Forgejo and a fake executor.
// Needs GATEWAY_TEST_DSN and GATEWAY_ADMIN_DSN as the approval tests do; make test-db runs it.
func TestConfirmationPage(t *testing.T) {
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
	h := newHarness(t, pool)
	if err := h.store.Grant(ctx, "cluster-admin", "technician", "create_ticket"); err != nil {
		t.Fatal(err)
	}
	// Proposals are made as the model's tool call will make them, with the proposer's role in context.
	propose := func(user, args string) approval.Proposal {
		t.Helper()
		pctx := withSession(ctx, session{identity: identity{user: user, role: "technician"}})
		p, err := h.store.Propose(pctx, user, "create_ticket", []byte(args),
			[]approval.Source{{Path: "content/en/docs/concepts/overview.md", ChunkIndex: 3, ContentHash: strings.Repeat("ab", 32)}})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := propose("alice", `{"title":"Pump 3 leaks","body":"<script>alert(1)</script> evil‮txt.exe","priority":"high"}`)
	path := "/proposals/" + p.ID.String()

	alice := h.browser()
	if got := alice.get(path); got.status != http.StatusSeeOther || !strings.HasPrefix(got.location, "/login?") {
		t.Fatalf("a visitor should be sent to sign in: %+v", got)
	}
	if got := alice.signIn(path, "alice", "maintenance:technicians"); got.location != path {
		t.Fatalf("after sign-in, back to the proposal: %+v", got)
	}
	pg := alice.get(path)
	if pg.status != http.StatusOK {
		t.Fatalf("the proposal page: %d", pg.status)
	}
	for _, want := range []string{"title · written by the model", "Pump 3 leaks", "&lt;script&gt;alert(1)&lt;/script&gt;",
		"evil⟨U&#43;202E⟩txt.exe", p.ArgsSHA256, "content/en/docs/concepts/overview.md, chunk 3", "Confirm and file"} {
		if !strings.Contains(pg.body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(pg.body, "<script>") {
		t.Error("model-written text reached the page unescaped")
	}
	token := csrf(t, pg.body)

	// Bob sees alice's proposal exactly as he sees one that does not exist, and cannot confirm it.
	bob := h.browser()
	bob.signIn("/", "bob", "maintenance:technicians")
	theirs, none := bob.get(path), bob.get("/proposals/00000000-0000-0000-0000-000000000000")
	if theirs.status != http.StatusNotFound || theirs.body != none.body || strings.Contains(theirs.body, "Pump 3") {
		t.Errorf("bob and alice's proposal: %d", theirs.status)
	}
	if got := bob.post(path+"/confirm", url.Values{"csrf": {csrf(t, bob.get("/").body)}}, nil); got.status != http.StatusNotFound {
		t.Errorf("bob confirming alice's proposal: %d", got.status)
	}

	// Forms without the session's token, or from another site, are refused.
	for name, c := range map[string]struct {
		form    url.Values
		headers map[string]string
	}{
		"no CSRF token":      {url.Values{}, nil},
		"bob's CSRF token":   {url.Values{"csrf": {csrf(t, bob.get("/").body)}}, nil},
		"another origin":     {url.Values{"csrf": {token}}, map[string]string{"Origin": "http://evil.example"}},
		"a cross-site fetch": {url.Values{"csrf": {token}}, map[string]string{"Sec-Fetch-Site": "cross-site"}},
	} {
		if got := alice.post(path+"/confirm", c.form, c.headers); got.status != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, got.status)
		}
	}
	if got := h.browser().post(path+"/confirm", url.Values{"csrf": {token}}, nil); got.status != http.StatusUnauthorized {
		t.Errorf("no session: %d", got.status)
	}
	if n := len(h.executor.calls()); n != 0 {
		t.Fatalf("the executor was called %d times by refused forms", n)
	}

	// The genuine confirmation: the executor gets a token and exactly the stored arguments, and the
	// browser never sees the token.
	done := alice.post(path+"/confirm", url.Values{"csrf": {token}}, nil)
	if done.status != http.StatusOK || !strings.Contains(done.body, "ticket #7") {
		t.Fatalf("confirming: %d %s", done.status, done.body)
	}
	calls := h.executor.calls()
	if len(calls) != 1 {
		t.Fatalf("executor calls: %d", len(calls))
	}
	sent := calls[0]
	sentArgs, _ := json.Marshal(sent["args"])
	canonical, _ := approval.Canonical(sentArgs)
	if sent["user"] != "alice" || sent["tool"] != "create_ticket" || canonical != p.Args {
		t.Errorf("the executor got %v", sent)
	}
	tok, _ := sent["token"].(string)
	if len(tok) < 40 || strings.Contains(done.body, tok) || strings.Contains(pg.body, tok) {
		t.Error("the token was missing or reached the browser")
	}
	// The token the executor got is the one the approval core issued: executing it works once.
	pctx := withSession(ctx, session{identity: identity{user: "alice", role: "technician"}})
	if _, err := h.store.Execute(pctx, tok, "alice", "create_ticket", []byte(p.Args)); err != nil {
		t.Errorf("the token sent to the executor does not execute: %v", err)
	}

	// A second confirmation of the same proposal is refused before the executor.
	if again := alice.post(path+"/confirm", url.Values{"csrf": {token}}, nil); again.status != http.StatusForbidden ||
		!strings.Contains(again.body, "already confirmed") {
		t.Errorf("a second confirmation: %d", again.status)
	}

	// The executor's refusal, such as a team change since sign-in, is shown as such.
	h.executor.set(http.StatusForbidden, `{"denied":"denied: the user has no role"}`)
	p2 := propose("alice", `{"title":"Second","body":"b","priority":"low"}`)
	refused := alice.post("/proposals/"+p2.ID.String()+"/confirm", url.Values{"csrf": {token}}, nil)
	if refused.status != http.StatusForbidden || !strings.Contains(refused.body, "Refused at execution") {
		t.Errorf("an executor refusal: %d", refused.status)
	}
	// An executor that gives no result is not reported as success.
	h.executor.set(http.StatusInternalServerError, `{"error":"execution failed"}`)
	p3 := propose("alice", `{"title":"Third","body":"b","priority":"low"}`)
	if failed := alice.post("/proposals/"+p3.ID.String()+"/confirm", url.Values{"csrf": {token}}, nil); failed.status != http.StatusBadGateway {
		t.Errorf("an executor failure: %d", failed.status)
	}

	// An expired proposal is shown without the button, and cannot be confirmed.
	p4 := propose("alice", `{"title":"Fourth","body":"b","priority":"low"}`)
	h.clock.advance(h.store.ProposalTTL)
	if got := alice.get("/proposals/" + p4.ID.String()); !strings.Contains(got.body, "has expired") || strings.Contains(got.body, "Confirm and file") {
		t.Error("an expired proposal still offers confirmation")
	}
	if got := alice.post("/proposals/"+p4.ID.String()+"/confirm", url.Values{"csrf": {token}}, nil); got.status != http.StatusForbidden {
		t.Errorf("confirming an expired proposal: %d", got.status)
	}

	// Signing out ends the session.
	if got := alice.post("/logout", url.Values{"csrf": {token}}, nil); got.status != http.StatusSeeOther {
		t.Errorf("sign out: %d", got.status)
	}
	if got := alice.get(path); got.status != http.StatusSeeOther {
		t.Error("the session outlived sign-out")
	}
	if n := len(h.executor.calls()); n != 3 {
		t.Errorf("executor calls: %d, want 3", n)
	}
}
