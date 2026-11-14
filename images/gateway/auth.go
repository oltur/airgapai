package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"airgapai/gateway/directory"
)

// oidc signs users in through Forgejo with the authorization code flow and PKCE. The browser goes to
// browserURL to sign in. The code is exchanged at internalURL, inside the cluster, with the client
// secret, and the user's name and teams are read from the ID token in that answer. No Forgejo token is
// kept: an OAuth access token could act as the user, and only the executor may file tickets.
type oidc struct {
	browserURL, internalURL string // Forgejo, as the browser and as the cluster reach it
	issuer                  string // what the ID token must name: Forgejo's ROOT_URL, without the slash
	clientID, clientSecret  string
	redirectURL             string // the gateway's /callback, as the browser reaches it
	org                     string // the organisation whose teams are the roles
	client                  *http.Client
	now                     func() time.Time
}

// identity is who signed in, as the ID token says.
type identity struct {
	user, role string
	expires    time.Time
}

func random() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeURL is where the browser signs in.
func (o *oidc) authorizeURL(state, nonce, verifier string) string {
	q := url.Values{"client_id": {o.clientID}, "redirect_uri": {o.redirectURL}, "response_type": {"code"},
		"scope": {"openid profile groups"}, "state": {state}, "nonce": {nonce},
		"code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"}}
	return o.browserURL + "/login/oauth/authorize?" + q.Encode()
}

// errLogin is a sign-in that did not succeed. Its text is shown to the user.
type errLogin struct{ reason string }

func (e *errLogin) Error() string { return e.reason }

// exchange swaps a code for an ID token and returns the identity it names, after checking the
// issuer, the audience, the expiry and the nonce. The signature is not checked: the token comes
// straight from Forgejo's token endpoint, on a request authenticated with the client secret, which is
// what OpenID Connect allows in place of a signature check, but over plain HTTP inside the cluster,
// which it does not. ADR 0017 states this.
func (o *oidc) exchange(ctx context.Context, code, verifier, nonce string) (identity, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {o.redirectURL},
		"client_id": {o.clientID}, "client_secret": {o.clientSecret}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.internalURL+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return identity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return identity{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return identity{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return identity{}, &errLogin{"Forgejo did not accept the sign-in code"}
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.IDToken == "" {
		return identity{}, &errLogin{"Forgejo's answer held no ID token"}
	}
	parts := strings.Split(tok.IDToken, ".")
	if len(parts) != 3 {
		return identity{}, &errLogin{"the ID token is malformed"}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identity{}, &errLogin{"the ID token is malformed"}
	}
	var claims struct {
		Iss      string          `json:"iss"`
		Aud      json.RawMessage `json:"aud"`
		Exp      int64           `json:"exp"`
		Nonce    string          `json:"nonce"`
		Username string          `json:"preferred_username"`
		Groups   []string        `json:"groups"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return identity{}, &errLogin{"the ID token is malformed"}
	}
	// aud is a string or a list of strings.
	var aud []string
	if err := json.Unmarshal(claims.Aud, &aud); err != nil {
		var one string
		if json.Unmarshal(claims.Aud, &one) != nil {
			return identity{}, &errLogin{"the ID token is malformed"}
		}
		aud = []string{one}
	}
	expires := time.Unix(claims.Exp, 0)
	switch {
	case claims.Iss != o.issuer:
		return identity{}, &errLogin{"the ID token is from another issuer"}
	case !slices.Contains(aud, o.clientID):
		return identity{}, &errLogin{"the ID token is for another client"}
	case !o.now().Before(expires):
		return identity{}, &errLogin{"the ID token has expired"}
	case subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1:
		return identity{}, &errLogin{"the ID token does not match this sign-in"}
	case !directory.UserName.MatchString(claims.Username):
		return identity{}, &errLogin{"this user name is not accepted: only lower-case names are"}
	}
	return identity{user: claims.Username, role: directory.RoleFromGroups(o.org, claims.Groups), expires: expires}, nil
}

// session is a signed-in browser. It lasts until the ID token's expiry, at most sessionTTL, and only
// in this process's memory, so a restart signs everyone out.
type session struct {
	identity
	csrf string
}

// pending is a sign-in in progress, between the redirect to Forgejo and the callback.
type pending struct {
	verifier, nonce, next string
	expires               time.Time
}

const (
	sessionTTL = time.Hour
	loginTTL   = 10 * time.Minute
)

type sessions struct {
	mu       sync.Mutex
	active   map[string]*session // by cookie value
	starting map[string]pending  // by state
	now      func() time.Time
}

func newSessions(now func() time.Time) *sessions {
	return &sessions{active: map[string]*session{}, starting: map[string]pending{}, now: now}
}

// prune drops what has expired. Called with mu held.
func (s *sessions) prune() {
	now := s.now()
	for k, v := range s.active {
		if !now.Before(v.expires) {
			delete(s.active, k)
		}
	}
	for k, v := range s.starting {
		if !now.Before(v.expires) {
			delete(s.starting, k)
		}
	}
}

// begin records a new sign-in and returns its state.
func (s *sessions) begin(next string) (state string, p pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	state = random()
	p = pending{verifier: random(), nonce: random(), next: next, expires: s.now().Add(loginTTL)}
	s.starting[state] = p
	return state, p
}

// finish takes a sign-in out, so that each state works once.
func (s *sessions) finish(state string) (pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	p, ok := s.starting[state]
	delete(s.starting, state)
	return p, ok
}

// start creates a session for who and returns its cookie value.
func (s *sessions) start(who identity) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	if limit := s.now().Add(sessionTTL); who.expires.After(limit) {
		who.expires = limit
	}
	id := random()
	s.active[id] = &session{identity: who, csrf: random()}
	return id
}

func (s *sessions) get(id string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	v, ok := s.active[id]
	if !ok {
		return session{}, false
	}
	return *v, true
}

func (s *sessions) end(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, id)
}

// sessionRoles gives the approval core the role of the signed-in user making this request, taken from
// their ID token at sign-in. The executor asks Forgejo again at execution, so a team change since
// sign-in still stops the call there.
type sessionRoles struct{}

type roleKey struct{}

func withSession(ctx context.Context, s session) context.Context {
	return context.WithValue(ctx, roleKey{}, s.identity)
}

func (sessionRoles) Role(ctx context.Context, user string) (string, error) {
	who, ok := ctx.Value(roleKey{}).(identity)
	if !ok || who.user != user {
		return "", fmt.Errorf("no signed-in session for this user")
	}
	return who.role, nil
}
