# 0018: The chat loop and its two tools

**Status:** accepted
**Date:** 2026-11-14

## Context

ADRs 0015 to 0017 built the path from a proposal to a filed ticket. A person signs in and confirms on a
page, and the executor files the ticket. Proposals were still made by an administrator command. The
plan asks for a model that answers from the manuals and can call two tools: `search_manuals`, which is
read-only, and `create_ticket`, which only proposes. The token must stay out of the model's context. It
also asks for the confirmation page to show the source chunks.

The model is Ministral 3 3B Instruct (Apache-2.0, ADR 0010), served by llama.cpp with `--jinja` for tool calls. It runs on CPU with a
4,096-token context and one request at a time. ADR 0011 measured about 55 prompt tokens and 15
generated tokens per second in the VM.

## Decision

- **The gateway runs the loop, server-side, one question per turn.** It sends a system prompt, the
  question and the two tool definitions, at temperature 0, and runs the tools the model calls. It
  returns their results, until the model answers or four calls have been made. There is no history
  between turns yet.
- **`search_manuals` searches with the signed-in user's role from the session.** The model can only
  send a query of up to 500 characters. Arguments are decoded with unknown fields refused, so a
  `"role": "engineer"` the model adds is an error, not an escalation. Each search returns 3 passages, to
  fit the context. They are numbered across the turn, so the answer can cite them as [1], [2].
- **`create_ticket` only calls `Propose`.** The proposal's sources are the passages the gateway's own
  searches found in that turn, recorded with their path, chunk index and hash. They are never anything
  the model says about its sources. The model is told the proposal's ID and that it waits for the
  user's confirmation and is not filed. A refusal, such as a role without the grant or an argument the
  tool does not take, goes back to the model as an error.
- **The loop has no path to a confirmation.** `chat.go` holds no call to `Confirm`, `Execute` or the
  executor, and a test checks the source for those names. The only way to confirm is the user's click on
  the proposal page, a POST with the session's CSRF token. The token is made there and goes straight to
  the executor.
- **The chat page renders the model's answer as text.** It is escaped, marked as written by the model,
  and shown with the same code point display for hidden characters as the confirmation page. It also
  lists each search, each proposal with a link to review it, each refused tool call, and the passages
  found, with their labels.
- **The confirmation page shows each source's text, read with the viewer's role.** The retrieval service
  gets `POST /chunks`, which looks chunks up by path, index and hash in the same `SET LOCAL ROLE`
  transaction as a search. A chunk the role cannot read, or whose text has changed since, comes back
  without text, and the page says so. Search results now also carry each chunk's hash.
- **Limits:**
  - each model call: 3 minutes and 512 generated tokens
  - a whole turn: 6 minutes
  - a question: 2,000 characters
  - a tool's arguments: 8 KiB
  - a tool result sent to the model: 16 KiB
- **Network:** the gateway reaches the chat model and the retrieval service. Nothing else gets new paths.
- **The `propose` administrator command stays for now.** The confirmation page's VM checks need
  proposals made without the model's choices. A scripted model that stands in for a compromised one
  comes next, and it will make them instead.

## Alternatives

- **The model names the role for a search.** A model the documents can instruct would choose
  `engineer`. The role comes from the session, and the model has no field for it.
- **The model lists its sources in `create_ticket`.** It could name restricted chunks it never saw, or
  leave out the ones it used. The gateway records what its own searches returned.
- **Chunk text stored with the proposal.** It would put restricted text into the gateway database and
  make it visible to whoever reads proposals. Reading it again with the viewer's role keeps row-level
  security as the only gate.
- **Streaming or a page that polls.** A turn takes tens of seconds on CPU. A plain form post that waits
  needs no script, which the Content Security Policy forbids.

## Consequences

- **A turn is slow,** and the page waits for it. Only one turn runs at a time, because llama.cpp serves
  one request at a time, and other users' turns queue behind it.
- **Three passages per search.** More would not fit the context with the system prompt, the tools and an
  answer. Recall at k=3 was not measured; ADR 0014 measured k of 5 and more.
- **The answer is not checked.** Whether it cites its passages, or refuses when they do not answer, is
  up to the model for now. Checking citations and validating output come in a later slice.
- **Sources are every passage the turn found,** not only the ones the ticket drew on.
- **The passages go to the model in full.** For an engineer, that includes restricted ones. Keeping
  restricted text out of a ticket that lower roles can read comes in a later slice.

### What this does not prove

- **That the model follows the system prompt.** It can still be instructed by the passages it reads.
  What the gateway enforces, whatever the model does, is that searches use the session's role, that
  proposals are only proposals, and that nothing is filed without the user's confirmation. The
  adversarial tests, with a stub that acts as a fully compromised model and with poisoned documents,
  come next.
- **That answers are correct or grounded.** That is the eval gate's job, in Phase 4.

## Measurements

`make test-db` ran the gateway's chat tests against a scripted model and a fake retrieval service, and
they passed:

- In one turn the model tried a search with its own `"role": "engineer"`, which was refused. Its plain
  search went to retrieval as the session's technician, and was the only search that reached
  retrieval. Its proposal was recorded with the two passages the search returned, with their hashes.
- What the model was sent included the numbered passages, the proposal's ID and the words "it is not
  filed". It did not include the session's CSRF token or the restricted passage.
- The answer's `<script>` was escaped. The turn reached the executor zero times.
- The proposal page showed the sources' text, escaped, read as the technician. A restricted source and
  one with a stale hash were listed without text.
- A model that only called tools was stopped after four calls. An unknown tool and an argument
  `create_ticket` does not take were refused, with no proposal. A model server error gave 502. A form
  without the CSRF token gave 403, and a question over 2,000 characters gave 400.
- Bob, an engineer without the grant, searched as an engineer, and his proposal was refused.
- The retrieval service's tests against the database returned a chunk only to a role that may read it,
  and only with its recorded hash. With no role, nothing was returned. An unknown role, an unknown
  field and more than 20 chunks were refused.

Each check was broken in turn, and a test failed every time:
- searching with another role
- accepting unknown arguments
- recording no sources
- a fifth model call
- not reporting an unknown tool as refused
- reading source text with another role
- showing text for a source that did not match
- the question's length limit

A copy of `chat.go` meant for restoring between mutations first went into an existing directory of the
same name, so the mutations piled up. They were rerun one at a time from a clean copy, and these are
those results.
