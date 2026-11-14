# 0017: Sign-in through Forgejo, and the confirmation page

**Status:** accepted
**Date:** 2026-11-14

## Context

ADRs 0015 and 0016 built the decision and the executor, but nothing authenticated the user: the user was
a field in the request, and a confirmation was made with an administrator command that printed its
token. The plan asks for a confirmation from an authenticated user, in a session separate from chat,
on a minimal server-rendered page built from the canonical proposal rather than model prose, with
model-written fields marked and sources shown, and with the token kept out of the model's context.
Forgejo already holds the users and their teams, and it is an OpenID Connect provider.

## Decision

- **A service, `gateway`, in the `gateway` namespace,** built from `images/gateway`, where the approval
  core also lives. It is where a person signs in and confirms. Later slices add the chat loop to it.
- **Sign-in through Forgejo, with the authorization code flow, PKCE (S256), `state` and `nonce`.** The
  gateway is a confidential OAuth client. The browser signs in at Forgejo through its own tunnel. The
  gateway exchanges the code inside the cluster, with the client secret, and checks the ID token's
  issuer, audience, expiry and nonce. The `state` is single-use and also kept in a cookie scoped to
  `/callback`, so a callback started in another browser signs nobody in. After sign-in, the browser
  returns only to the start page or to a proposal's address, never elsewhere.
- **The ID token is not signature-checked.** It comes straight from Forgejo's token endpoint, on a request
  authenticated with the client secret. OpenID Connect allows that in place of a signature check when
  the channel is TLS. Here it is plain HTTP inside the cluster, so anything that can tamper with
  in-cluster traffic could forge an identity. Checking the signature would not fix that on its own, as
  the keys would arrive over the same channel. TLS inside the cluster is a later step for every service.
- **The user name and role come from the ID token.** `preferred_username` must be lower case, as in ADR
  0016. The role is taken from the `groups` claim, in which Forgejo lists each team as
  `maintenance:technicians` or `maintenance:engineers`, using the same mapping as the executor. A small
  shared package, `images/gateway/directory`, holds both rules so the two services cannot drift apart.
- **No Forgejo token is kept.** The access token from the exchange is discarded. Asked for only `openid
  profile groups`, it could still file a ticket and create a repository as the user; see Measurements.
  Keeping it would make the gateway a second holder of a credential that files tickets. The executor
  stays the only one.
- **The role at confirmation comes from the session, and the executor asks Forgejo again.** Confirm
  checks the role from the ID token at sign-in. The executor's check, a moment later, asks Forgejo for
  the current teams, as in ADR 0016. A team change since sign-in therefore stops the call there.
- **Sessions are kept in memory, with one replica.** A random 256-bit cookie value, `HttpOnly` and
  `SameSite=Lax`, maps to the user, their role and a CSRF token. At most 1,000 sign-ins may be in
  progress at once, each for ten minutes, since anyone who reaches the gateway can start one. Expired
  entries are dropped at most once a minute, and every lookup checks its own entry's expiry. A session ends at the ID token's expiry,
  after an hour at most, at sign-out, or when the gateway restarts. `Secure` is set only when the
  gateway's address is HTTPS, since the lab's browser address is `http://127.0.0.1:18092`.
- **Every form is checked three ways:** a live session, the session's CSRF token, and, where the browser
  sends them, `Origin` equal to the gateway's own and `Sec-Fetch-Site: same-origin`.
- **The page is rendered from the stored proposal only:** its tool, its ID, its argument hash, its expiry,
  and each argument from the canonical JSON. Each argument sits in a box labelled "written by the
  model", in preformatted text, escaped by `html/template`. Values that are not strings are shown as
  the canonical text holds them. Characters that hide, disguise or reorder text are shown as their code
  points, such as `⟨U+202E⟩` for a right-to-left override: every code point Go does not count as
  graphic, which takes in controls, format and tag characters, line and paragraph separators,
  private-use and unassigned code points, and also variation selectors, the other default-ignorable
  characters such as Hangul fillers, and the replacement character. Lookalike letters from other
  scripts are visible characters, and are not flagged. The sources are listed by path, chunk index and hash. An
  expired proposal is shown without the button.
- **Another user's proposal looks exactly like one that does not exist.** It gets the same 404 page,
  with none of its text.
- **On Confirm, the token goes straight to the executor.** The gateway calls `Confirm`, then the
  executor with the token, the user, and the stored tool and canonical arguments, and shows the result:
  the ticket number, the executor's refusal, or that no result came back. The token exists only in that
  request's memory. It is not logged, not sent to the browser, and a model can never reach it. Once
  confirmed, the work runs to its end even if the browser goes away, so the token is not lost unused.
- **Headers:** a Content Security Policy with no scripts, forms only to the gateway, no framing,
  `Referrer-Policy: no-referrer`, so proposal addresses and sign-in codes do not reach Forgejo, and
  `Cache-Control: no-store`.
- **The executor's `confirm` command is removed.** `propose` stays, standing in for the model's tool
  call until the chat loop exists.
- **Bootstrap registers the OAuth client** as the Forgejo administrator, with the one redirect address
  `http://127.0.0.1:18092/callback`. It puts the client's ID and secret, with the gateway database
  password, in the `gateway` Secret, and never prints them.
- **Network:** the gateway reaches PostgreSQL, Forgejo and the executor. Only the gateway may call the
  executor, and nothing in the cluster may call the gateway: the browser comes through
  `kubectl port-forward`.

- **The Go services move to Go 1.27.4.** govulncheck found GO-2026-6859, an HTTP/1 server crash in
  `net/http`'s `MaxBytesReader`, reachable from the gateway, the executor's module, retrieval and
  ingestion under 1.27.3. Their images were rebuilt, and each built twice to the same new digest.

## Alternatives

- **Roles read with the user's access token at each confirmation.** This would be live, but it would mean
  keeping that token, which can act as the user in Forgejo.
- **A Forgejo account for the gateway, to read teams.** It would need to be an organisation owner, as ADR
  0016 found, so it would be a second privileged credential, only to repeat a check the executor makes
  anyway.
- **Sessions in the database.** They would survive restarts and allow more than one replica, but would
  need a new table and grants for something a one-replica lab does not need yet.
- **Show the token to the user and have them paste it.** That is how the administrator command worked.
  It puts the token on screen and in the clipboard, where a compromised chat page could read it.

## Consequences

- **A restart of the gateway signs everyone out,** and a sign-in in progress fails.
- **The browser needs two tunnels and one hosts entry:** the gateway on `127.0.0.1:18092`, and Forgejo on
  `forgejo.forgejo.svc:13000`, a name that must resolve to `127.0.0.1`. Forgejo's `ROOT_URL` stays its
  in-cluster address, which Argo CD and the CI runner clone from.
- **Forgejo asks each user once whether to authorise the gateway.**
- **A confirmed call that does not run cannot be retried.** When the executor refuses, for instance
  because the user has since left their team, or gives no answer, the token may be unused, but the
  gateway does not keep it, and the proposal cannot be confirmed twice. The page says so, and the call
  has to be proposed again. Keeping tokens for a retry would mean holding them beyond the request.
- **Cookies on `127.0.0.1` are shared by port.** Browsers do not separate cookies by port, so another
  service tunnelled to `127.0.0.1` gets the gateway's cookies and could set its own under their names.
  The names are specific to the gateway, which avoids collisions but not a hostile service on the
  operator's machine. On its own HTTPS host, the gateway would use `Secure` cookies with the `__Host-`
  prefix.
- **The sources show no text yet.** The proposal stores chunk identities and hashes. Showing the chunk
  text, with the reader's role, comes with the chat loop, which is also where sources start being
  recorded from real search results rather than from the caller.

### What this does not prove

- **That a misled user does not confirm a harmful request.** The page shows exactly what will be filed,
  but it cannot make anyone read it. This is a stated residual risk.
- **That the identity holds against someone who can tamper with traffic inside the cluster.** See the
  signature decision above.
- **That users file tickets only through the gateway.** A technician can open an issue in
  `maintenance/tickets` in Forgejo directly, as the measurements show. That is the person acting, not
  the model, and the claim is about what a model can cause.
- **That only the gateway can confirm.** Anyone with the gateway database password, or control of the
  cluster, can write a confirmation row. The claim is about the paths that a model or a browser user
  has.
- **Replay, altered arguments or another user's token in the VM.** The gateway sends only the stored
  arguments and never reveals the token, so those bindings cannot be tried through it. They are tested
  against the cluster's schema in `make test-db`, as in ADR 0015.

## Measurements

On 2026-11-14, against the lab's Forgejo 17.0.1 with a temporary OAuth client, which was deleted
afterwards, signing in as alice by script:

- The discovery document names `openid`, `profile`, `email` and `groups` as scopes, and RS256 for ID
  tokens.
- Forgejo showed its authorisation page on the first sign-in.
- The token response held an access token, a refresh token and an ID token, with `expires_in` 3600.
- The ID token's `groups` were `["maintenance", "maintenance:technicians"]`, so the private
  organisation's teams are included. `iss` was `http://forgejo.forgejo.svc:3000`, its `ROOT_URL`.
- Forgejo's sign-in form has no CSRF field. A form post with an `Origin` header signed in.
- With the access token from that exchange, the API answered `GET /user` with 200, filed an issue in
  `maintenance/tickets` as alice (201), and created a repository for her (201). Both were deleted as
  the administrator straight after.

`make test-db` ran the gateway's tests against a fake Forgejo token endpoint and a fake executor, and
they passed:

- Sign-in: an ID token from another issuer, for another client, expired, with another sign-in's
  nonce, or naming an upper-case user or one with a slash was refused, and no session was created. A
  callback from another browser, a code used twice and a state used twice were refused. Four
  addresses outside the site, as the return path, were replaced by the start page. A session ended
  after an hour even though the ID token lasted eight.
- The page: a visitor was sent to sign in. The page marked the model's fields, escaped
  `<script>alert(1)</script>`, showed the right-to-left override as `⟨U+202E⟩`, and listed the source.
  Bob got the same 404 page for alice's proposal as for one that does not exist, and could not confirm
  it. Forms with no CSRF token, with bob's token, from another origin, or from a cross-site fetch were
  refused, and with no session got 401. None of them reached the executor.
- Confirming: the executor got the user, the tool and exactly the stored canonical arguments, with a
  token that the approval core then accepted, and that appeared in no page. A second confirmation was
  refused as already confirmed. An executor refusal and an executor failure were shown as such, not as
  success. An expired proposal had no button, and confirming it was refused. Sign-out ended the session.

After the review, the code point display was widened, as above, and tested on variation selectors,
tag characters, separators, Hangul fillers, private-use, unassigned and replacement characters, with a
non-breaking space left as it is. A number with 20 digits, `1.0` and an object with `<&>` were shown
as written. A thousand sign-ins in progress were allowed, the next got 503, and once they had expired
there was room again. A sign-in and a session each ended at their expiry less than a minute after a
prune.

Each check was then broken in turn, and the tests failed for every one: the issuer, the audience, the
expiry, the nonce, the user name, the one-hour cap, single use of the state, the state cookie, the return
path, the code point display, the Content Security Policy, the CSRF token, the `Origin` check, the
`Sec-Fetch-Site` check, sending the stored arguments, showing an executor refusal, not taking a failure
for success, escaping the model's text, and, after the review, the non-graphic and
variation-selector rules, raw values, the bound on sign-ins, and each entry's own expiry check. The cap and single use of the state first survived: the
fake ID token expired with the session, and the state's reuse was refused for its missing cookie. Both
tests were tightened until they failed.
