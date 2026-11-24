# Demo

One command shows the claim end to end, with the real model through the deployed gateway:

```sh
make demo   # about 3 minutes, on a VM that make rebuild built and tested
```

It runs `demo/demo.sh` in the VM, then copies the audit log off it. In five steps:

1. **Nothing leaves.** A pod with the gateway's network label reaches the chat model, as the
   control, and cannot reach `1.1.1.1:443` or resolve a public name.
2. **A technician gets an answer that cites the manuals,** or none at all.
3. **The same question, as two roles.** Its answer is on a page labelled restricted. The technician's
   searches find no restricted passage; the engineer's do.
4. **The model proposes a ticket, and only the user files it.** The turn files nothing, a
   confirmation without the session's CSRF token is refused, and the user's confirmation files
   exactly one ticket. The proposal's page then shows that ticket, and no second confirmation.
5. **Every decision is in the audit log,** whose chain verifies, and which is then copied off the VM.

## What it checks, and what it only shows

The demo fails if the stack breaks a rule it enforces: a path out, a restricted passage in a
technician's turn, an answer shown without citing what its turn found, a ticket filed without a
confirmation, or a broken audit chain. What the model writes, and whether it proposes a ticket or
finds the engineer's passages, is the model's choice. The demo prints it, and says so when it differs,
but does not fail on it.

So what reproduces is the rules, not necessarily the words. In five runs on 2026-11-24, every rule
held. The first three ran on the VM from the [evidence index](evidence.md)'s run R, and the last two
on a VM rebuilt from `3d70755`, with ADRs 0029 and 0030 deployed. Runs 2 to 5 gave the same model
output, word for word, across that rebuild. Run 1, the first turns after run R's evals, differed:
its first answer was worded differently, its passages came in another order, and the engineer's
answer was shown, where the later runs withheld it for citing nothing. ADR 0026's three eval runs
also gave identical answers. Why the first run differed has not been established.

The demo fails on a gateway rule, with the outcome it saw: an answer is `cited` only if every passage
it cites is one the page lists, and `withheld` or `stopped` when the gateway showed none. A ticket
count that Forgejo does not give fails the demo rather than passing a comparison. `make demo` copies
the audit log whether the demo passed or not.

It shows less than the tests: one question per step, a step's control only where it is cheap, and
none of the compromised-model, poisoned-document, egress-layer or eval cases. Those are `make
rebuild`'s, listed in the [evidence index](evidence.md).

## A recorded run

The fifth run, as printed. The answers and ticket text are the model's, cut at 400 and
200 characters.

```text
== 1. Nothing leaves
   A pod with the gateway's network label tries the chat model, which it may reach, and the internet.
ok   the gateway's label reaches the chat model (control)
ok   it cannot reach 1.1.1.1:443
ok   nor resolve a public name
   make test-egress opens each layer in turn to show which one holds; this is the closed state.

== 2. A technician gets an answer that cites the manuals
ok   alice, a technician, signs in through Forgejo
   alice asks: What is the difference between a resource request and a limit for a container?
   (the turn took 34 s, on CPU)
ok   the turn completed
   A **resource request** for a container specifies the minimum amount of a resource (e.g., CPU, memory) that the container expects to be allocated. It acts as a guarantee that the container will have enough resources to run smoothly, but the node may allocate more if available. A **limit** for a container sets the maximum amount of a resource that the container is allowed to use. Unlike requests, li …
   Passages found:
   - content/en/docs/concepts/configuration/manage-resources-containers.md, chunk 5, public
   - content/en/docs/concepts/configuration/manage-resources-containers.md, chunk 1, public
   - content/en/docs/concepts/policy/limit-range.md, chunk 1, public
ok   the answer cites passages the turn found, or none is shown (cited)

== 3. The same question, two roles
   The answer is in a page labelled restricted, a synthetic stand-in for text above a technician's role.
   alice asks: What are the differences between the Baseline and Restricted Pod Security Standards?
   (the turn took 43 s, on CPU)
ok   the turn completed
ok   a technician's turn found no restricted passage
   Passages found:
   - content/en/docs/concepts/policy/pid-limiting.md, chunk 1, public
   - content/en/docs/concepts/policy/pid-limiting.md, chunk 0, public
   - content/en/docs/concepts/services-networking/network-policies.md, chunk 2, public
   (no answer is shown)
ok   the answer cites passages the turn found, or none is shown (withheld)
ok   bob, an engineer, signs in
   bob asks: What are the differences between the Baseline and Restricted Pod Security Standards?
   (the turn took 42 s, on CPU)
ok   the turn completed
   Passages found:
   - content/en/docs/concepts/security/pod-security-standards.md, chunk 2, restricted
   - content/en/docs/concepts/security/pod-security-standards.md, chunk 0, restricted
   - content/en/docs/concepts/security/pod-security-standards.md, chunk 9, restricted
   (no answer is shown)
ok   the answer cites passages the turn found, or none is shown (withheld)

== 4. The model proposes a ticket; only the user files it
   alice asks: The web pods in hall 3 keep restarting after the last deployment. Search the manuals for why a container restarts, then file a high priority ticket about the restarts.
   (the turn took 30 s, on CPU)
ok   the turn completed
ok   nothing was filed by the turn
   The page says: A ticket was proposed, not filed.
   alice opens /proposals/57ac132a-11bd-44d4-a3d3-0d1492bac27a. It shows what will be filed, marked as the model's, and where:
   title: High-frequency restarts of web pods in hall 3
   priority: high
   body: Web pods in hall 3 are restarting frequently after deployment. Possible causes include memory or CPU resource exhaustion, leading to OOM (Out of Memory) kills or evictions. Investigate resource limits …
   repository: maintenance/tickets
   Sources shown with their text, read with her role: 3
ok   a confirmation without the session's CSRF token is refused
ok   and files nothing
ok   alice confirms
   ticket #4 was filed by the executor.
ok   exactly one ticket filed
   Back on the proposal's page: The executor filed it as ticket #4.
ok   the proposal's page shows the ticket it filed, and offers no second confirmation

== 5. Every decision is in the audit log
ok   the audit chain is unbroken
   52 rows, 4 of them written during this demo:
   propose  allowed  alice
   confirm  allowed  alice
   execute  allowed  alice
   result  succeeded  executor
   make demo now copies the log off the VM, where a later rewrite of these rows would show.

demo: every rule held
ok audit copy: 5 new rows, 52 in all, last hash 5e92af1676bcdc0ad1bb3656dbcbaf775fbc4c6ced6c5dc3e44a97b6db618394, in /mnt/data/airgapai/audit/gateway-audit-7711851951152590881.jsonl
```

Its four turns took 30 to 43 s each.
