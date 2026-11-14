// Gateway is where a person signs in, asks the model, and confirms what it proposed. It signs users in through
// Forgejo, shows a proposal built from its stored canonical arguments rather than from anything a
// model wrote about it, and on confirmation hands the one-time token straight to the executor. The
// token is never sent to the browser, and nothing a model can reach ever sees it. See ADR 0017.
//
// It serves the pages below, and GET /ready. The chat loop is in chat.go. The browser reaches it through kubectl port-forward;
// nothing in the cluster may call it yet.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"airgapai/gateway/approval"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type server struct {
	store     *approval.Store
	auth      *oidc
	sessions  *sessions
	executor  *executor
	chat      *chatter
	retrieval *retrieval // for the confirmation page's source text
	origin    string     // the gateway's own origin, as the browser reaches it
	secure    bool       // whether cookies need HTTPS
	ticketURL string     // Forgejo's issue list, as the browser reaches it
	now       func() time.Time
}

// Cookie names are specific to the gateway. Browsers do not separate cookies by port, so in the lab
// every service tunnelled to 127.0.0.1 shares them; see ADR 0017.
const (
	sessionCookie = "airgapai_gateway_session"
	loginCookie   = "airgapai_gateway_login"
)

// nextPath is where a sign-in may return to: the start page or a proposal, never another site.
var nextPath = regexp.MustCompile(`^/(chat|proposals/[0-9a-f-]{36})?$`)

// headers apply to every answer: no scripts, no framing, forms only to this origin, no referrer,
// which would carry proposal IDs and sign-in codes to Forgejo, and no caching.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("GET /callback", s.handleCallback)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /chat", s.handleChatPage)
	mux.HandleFunc("POST /chat", s.handleChat)
	mux.HandleFunc("GET /proposals/{id}", s.handleProposal)
	mux.HandleFunc("POST /proposals/{id}/confirm", s.handleConfirm)
	mux.HandleFunc("GET /ready", s.handleReady)
	return headers(mux)
}

func (s *server) cookie(name, value, path string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge, HttpOnly: true,
		Secure: s.secure, SameSite: http.SameSiteLaxMode}
}

// current is the request's session, if it has a live one.
func (s *server) current(r *http.Request) (string, session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", session{}, false
	}
	v, ok := s.sessions.get(c.Value)
	return c.Value, v, ok
}

// sameOrigin rejects a form posted from another site. Browsers send Origin with a POST, and
// Sec-Fetch-Site too; a client that sends neither still needs the session's CSRF token.
func (s *server) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != s.origin {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	return true
}

// checkForm is the guard on every POST: a live session, this origin, and the session's CSRF token.
func (s *server) checkForm(w http.ResponseWriter, r *http.Request) (string, session, bool) {
	return s.checkFormUpTo(w, r, 4<<10)
}

// checkFormUpTo is checkForm for a form of up to limit bytes.
func (s *server) checkFormUpTo(w http.ResponseWriter, r *http.Request, limit int64) (string, session, bool) {
	id, sess, ok := s.current(r)
	if !ok {
		s.render(w, http.StatusUnauthorized, "message", page{Title: "Not signed in", Message: "Your session has ended. Sign in and open the proposal again."})
		return "", session{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if !s.sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
		s.render(w, http.StatusForbidden, "message", page{Title: "Refused", Message: "This form did not come from this page."})
		return "", session{}, false
	}
	return id, sess, true
}

func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.current(r)
	p := page{Title: "Confirmations"}
	if ok {
		p.Session = &sess
	}
	s.render(w, http.StatusOK, "home", p)
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !nextPath.MatchString(next) {
		next = "/"
	}
	state, p, ok := s.sessions.begin(next)
	if !ok {
		s.render(w, http.StatusServiceUnavailable, "message", page{Title: "Busy", Message: "Too many sign-ins are in progress. Try again in a few minutes."})
		return
	}
	// The state is tied to this browser, so a callback started elsewhere cannot sign it in.
	http.SetCookie(w, s.cookie(loginCookie, state, "/callback", int(loginTTL.Seconds())))
	http.Redirect(w, r, s.auth.authorizeURL(state, p.nonce, p.verifier), http.StatusSeeOther)
}

func (s *server) handleCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, reason string) {
		log.Printf("sign-in refused: %s", reason)
		s.render(w, status, "message", page{Title: "Sign-in failed", Message: reason})
	}
	http.SetCookie(w, s.cookie(loginCookie, "", "/callback", -1))
	q := r.URL.Query()
	c, err := r.Cookie(loginCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(q.Get("state"))) != 1 {
		fail(http.StatusBadRequest, "this sign-in was not started in this browser")
		return
	}
	p, ok := s.sessions.finish(q.Get("state"))
	if !ok {
		fail(http.StatusBadRequest, "this sign-in has expired or was already used")
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		fail(http.StatusBadRequest, "Forgejo did not sign you in")
		return
	}
	who, err := s.auth.exchange(r.Context(), q.Get("code"), p.verifier, p.nonce) // the client bounds it
	var refused *errLogin
	switch {
	case errors.As(err, &refused):
		fail(http.StatusForbidden, refused.reason)
		return
	case err != nil:
		log.Printf("sign-in: %v", err)
		fail(http.StatusBadGateway, "Forgejo could not be reached")
		return
	}
	http.SetCookie(w, s.cookie(sessionCookie, s.sessions.start(who), "/", 0))
	http.Redirect(w, r, p.next, http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.checkForm(w, r)
	if !ok {
		return
	}
	s.sessions.end(id)
	http.SetCookie(w, s.cookie(sessionCookie, "", "/", -1))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// proposal loads the signed-in user's own proposal, or answers for it: a sign-in for a visitor, and
// for anyone else's or an unknown proposal, the same "not found".
func (s *server) proposal(w http.ResponseWriter, r *http.Request, sess session) (approval.Proposal, bool) {
	notFound := func() {
		s.render(w, http.StatusNotFound, "message", page{Title: "Not found", Session: &sess, Message: "You have no proposal with this address."})
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		notFound()
		return approval.Proposal{}, false
	}
	p, err := s.store.Proposal(r.Context(), sess.user, id)
	switch {
	case approval.IsDenied(err):
		notFound()
		return approval.Proposal{}, false
	case err != nil:
		log.Printf("loading a proposal: %v", err)
		s.render(w, http.StatusInternalServerError, "message", page{Title: "Error", Session: &sess, Message: "The proposal could not be loaded."})
		return approval.Proposal{}, false
	}
	return p, true
}

func (s *server) handleProposal(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/login?"+url.Values{"next": {r.URL.Path}}.Encode(), http.StatusSeeOther)
		return
	}
	p, ok := s.proposal(w, r, sess)
	if !ok {
		return
	}
	v, err := s.view(p, sess)
	if err == nil {
		s.sourceText(r.Context(), v.Proposal, sess, p.Sources)
	}
	if err != nil {
		log.Printf("rendering proposal %s: %v", p.ID, err)
		s.render(w, http.StatusInternalServerError, "message", page{Title: "Error", Message: "The proposal could not be shown."})
		return
	}
	s.render(w, http.StatusOK, "proposal", v)
}

// confirmTimeout bounds the confirmation and the executor's work, which go on if the browser leaves.
const confirmTimeout = 60 * time.Second

func (s *server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.checkForm(w, r)
	if !ok {
		return
	}
	p, ok := s.proposal(w, r, sess)
	if !ok {
		return
	}
	// Once confirmed, the token exists only here. Finish even if the browser goes away, so it reaches
	// the executor rather than being lost unused.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(withSession(r.Context(), sess)), confirmTimeout)
	defer cancel()
	token, err := s.store.Confirm(ctx, sess.user, p.ID)
	if approval.IsDenied(err) {
		s.render(w, http.StatusForbidden, "message", page{Title: "Not confirmed", Session: &sess, Message: err.Error()})
		return
	}
	if err != nil {
		log.Printf("confirming %s: %v", p.ID, err)
		s.render(w, http.StatusInternalServerError, "message", page{Title: "Error", Session: &sess, Message: "The confirmation could not be recorded, so nothing was filed."})
		return
	}
	res := s.executor.execute(ctx, token, sess.user, p.Tool, p.Args)
	switch {
	case res.err != nil:
		log.Printf("executing %s: %v", p.ID, res.err)
		s.render(w, http.StatusBadGateway, "message", page{Title: "Not filed", Session: &sess,
			Message: "The confirmation was recorded, but the executor did not report a result. Check the tickets before proposing it again."})
	case res.denied != "":
		// The executor's refusal leaves the token unused, but the proposal stays confirmed and the
		// token is dropped here, so the call cannot be retried; see ADR 0017.
		s.render(w, http.StatusForbidden, "message", page{Title: "Refused at execution", Session: &sess,
			Message: res.denied + ". Nothing was filed. This proposal cannot be confirmed again: once the reason is resolved, it has to be proposed anew."})
	default:
		s.render(w, http.StatusOK, "filed", page{Title: "Ticket filed", Session: &sess,
			Issue: res.issue, IssueURL: fmt.Sprintf("%s/%d", s.ticketURL, res.issue)})
	}
}

func (s *server) handleChatPage(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/login?next=%2Fchat", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "chat", page{Title: "Ask the manuals", Session: &sess})
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	// A question of maxQuestion characters, each up to 4 bytes and 12 once form-encoded, and the token.
	_, sess, ok := s.checkFormUpTo(w, r, 12*maxQuestion+1<<10)
	if !ok {
		return
	}
	question := strings.TrimSpace(r.PostFormValue("question"))
	if question == "" || len([]rune(question)) > maxQuestion {
		s.render(w, http.StatusBadRequest, "chat", page{Title: "Ask the manuals", Session: &sess,
			Message: fmt.Sprintf("A question needs 1 to %d characters.", maxQuestion)})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	defer cancel()
	t, err := s.chat.chat(ctx, sess, question)
	if errors.Is(err, errBusy) {
		s.render(w, http.StatusServiceUnavailable, "chat", page{Title: "Ask the manuals", Session: &sess,
			Message: "The model is answering another question. Try again when it has finished."})
		return
	}
	if err != nil {
		log.Printf("chat: %v", err)
		s.render(w, http.StatusBadGateway, "chat", page{Title: "Ask the manuals", Session: &sess, Turn: &t,
			Message: "The model did not answer. Any proposal made before that is listed below."})
		return
	}
	s.render(w, http.StatusOK, "chat", page{Title: "Ask the manuals", Session: &sess, Turn: &t})
}

func (s *server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, "ok\n")
}

// executor calls the executor, the only holder of the ticket credential.
type executor struct {
	url    string
	client *http.Client
}

type outcome struct {
	issue  int64
	denied string
	err    error
}

func (e *executor) execute(ctx context.Context, token, user, tool, args string) outcome {
	body, err := json.Marshal(map[string]any{"token": token, "user": user, "tool": tool, "args": json.RawMessage(args)})
	if err != nil {
		return outcome{err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/execute", bytes.NewReader(body))
	if err != nil {
		return outcome{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return outcome{err: err}
	}
	defer resp.Body.Close()
	var answer struct {
		Issue  int64  `json:"issue"`
		Denied string `json:"denied"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&answer); err != nil {
		return outcome{err: fmt.Errorf("executor answered %d: %w", resp.StatusCode, err)}
	}
	switch {
	case resp.StatusCode == http.StatusOK && answer.Issue > 0:
		return outcome{issue: answer.Issue}
	case resp.StatusCode == http.StatusForbidden && answer.Denied != "":
		return outcome{denied: answer.Denied}
	}
	return outcome{err: fmt.Errorf("executor answered %d", resp.StatusCode)}
}

func main() {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	addr := fs.String("listen", ":8080", "address to serve on")
	_ = fs.Parse(os.Args[1:])
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	s, err := newServer(pool, os.Getenv, time.Now)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: *addr, Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: turnTimeout + 10*time.Second}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// newServer reads its settings from env. Every URL is required, so a missing one fails at start.
func newServer(pool *pgxpool.Pool, env func(string) string, now func() time.Time) (*server, error) {
	need := map[string]string{}
	for _, k := range []string{"GATEWAY_URL", "FORGEJO_URL", "FORGEJO_BROWSER_URL", "EXECUTOR_URL", "CHAT_URL", "RETRIEVAL_URL",
		"OAUTH_CLIENT_ID", "OAUTH_CLIENT_SECRET"} {
		if need[k] = strings.TrimSuffix(env(k), "/"); need[k] == "" {
			return nil, fmt.Errorf("%s is not set", k)
		}
	}
	gw, err := url.Parse(need["GATEWAY_URL"])
	if err != nil || gw.Scheme == "" || gw.Host == "" {
		return nil, fmt.Errorf("GATEWAY_URL is not an absolute URL")
	}
	org := env("TICKETS_ORG")
	if org == "" {
		org = "maintenance"
	}
	repo := env("TICKETS_REPO")
	if repo == "" {
		repo = "tickets"
	}
	client := &http.Client{Timeout: 15 * time.Second,
		// Forgejo's token endpoint and the executor answer directly; a redirect would be unexpected.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	store := approval.New(pool, sessionRoles{}, []approval.Tool{approval.CreateTicket}, now)
	rv := &retrieval{url: need["RETRIEVAL_URL"], client: &http.Client{}}
	return &server{
		store: store,
		chat: &chatter{model: &model{url: need["CHAT_URL"], client: &http.Client{}}, retrieval: rv, busy: make(chan struct{}, 1),
			propose: func(ctx context.Context, sess session, args []byte, sources []approval.Source) (approval.Proposal, error) {
				return store.Propose(withSession(ctx, sess), sess.user, "create_ticket", args, sources)
			}},
		auth: &oidc{browserURL: need["FORGEJO_BROWSER_URL"], internalURL: need["FORGEJO_URL"], issuer: need["FORGEJO_URL"],
			clientID: need["OAUTH_CLIENT_ID"], clientSecret: need["OAUTH_CLIENT_SECRET"],
			redirectURL: need["GATEWAY_URL"] + "/callback", org: org, client: client, now: now},
		sessions:  newSessions(now),
		executor:  &executor{url: need["EXECUTOR_URL"], client: &http.Client{Timeout: confirmTimeout}},
		retrieval: rv,
		origin:    gw.Scheme + "://" + gw.Host,
		secure:    gw.Scheme == "https",
		ticketURL: need["FORGEJO_BROWSER_URL"] + "/" + url.PathEscape(org) + "/" + url.PathEscape(repo) + "/issues",
		now:       now,
	}, nil
}
