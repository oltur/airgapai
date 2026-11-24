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
   exactly one ticket.
5. **Every decision is in the audit log,** whose chain verifies, and which is then copied off the VM.

## What it checks, and what it only shows

The demo fails if the stack breaks a rule it enforces: a path out, a restricted passage in a
technician's turn, an answer shown without citing what its turn found, a ticket filed without a
confirmation, or a broken audit chain. What the model writes, and whether it proposes a ticket or
finds the engineer's passages, is the model's choice. The demo prints it, and says so when it differs,
but does not fail on it.

So what reproduces is the rules, not the words. In two runs on 2026-11-24, one after the other on the
VM from the [evidence index](evidence.md)'s run R, every rule held. The model's words did not repeat:
the first answer was worded differently, its passages came in another order, and the engineer's
answer was shown in the first run and withheld in the second, for citing nothing. ADR 0026's three eval
runs gave identical answers. Why these two runs did not has not been established.

It shows less than the tests: one question per step, a step's control only where it is cheap, and
none of the compromised-model, poisoned-document, egress-layer or eval cases. Those are `make
rebuild`'s, listed in the [evidence index](evidence.md).

## A recorded run

The first of the two runs, as printed. The answers and ticket text are the model's, cut at 400 and
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
   (the turn took 41 s, on CPU)
ok   the turn completed
   The manuals do not provide a detailed comparison of resource requests and limits for containers in a single passage. However, here is the key distinction based on the available information: - **Resource Requests**: These define the minimum amount of CPU, memory, or other resources a container expects to run. They act as a guarantee to the Kubernetes scheduler that the container will have enough re …
   Passages found:
   - content/en/docs/concepts/configuration/manage-resources-containers.md, chunk 5, public
   - content/en/docs/concepts/policy/limit-range.md, chunk 1, public
   - content/en/docs/concepts/configuration/manage-resources-containers.md, chunk 1, public
ok   the answer cites passages the turn found, or is withheld

== 3. The same question, two roles
   The answer is in a page labelled restricted, a synthetic stand-in for text above a technician's role.
   alice asks: What are the differences between the Baseline and Restricted Pod Security Standards?
   (the turn took 42 s, on CPU)
ok   the turn completed
ok   a technician's turn found no restricted passage
   Passages found:
   - content/en/docs/concepts/policy/pid-limiting.md, chunk 1, public
   - content/en/docs/concepts/policy/pid-limiting.md, chunk 0, public
   - content/en/docs/concepts/services-networking/network-policies.md, chunk 2, public
   (no answer is shown)
ok   the answer cites passages the turn found, or is withheld
ok   bob, an engineer, signs in
   bob asks: What are the differences between the Baseline and Restricted Pod Security Standards?
   (the turn took 38 s, on CPU)
ok   the turn completed
   Passages found:
   - content/en/docs/concepts/security/pod-security-standards.md, chunk 2, restricted
   - content/en/docs/concepts/security/pod-security-standards.md, chunk 0, restricted
   - content/en/docs/concepts/security/pod-security-standards.md, chunk 9, restricted
   The **Baseline** and **Restricted** Pod Security Standards differ primarily in their level of restriction and focus: - **Baseline** ([1], [2]): - Designed for ease of adoption for common workloads. - Prevents known privilege escalations while allowing minimally specified Pod configurations. - Does not enforce stricter controls like running containers as non-root or restricting volume types. - **Re …
ok   the answer cites passages the turn found, or is withheld

== 4. The model proposes a ticket; only the user files it
   alice asks: The web pods in hall 3 keep restarting after the last deployment. Search the manuals for why a container restarts, then file a high priority ticket about the restarts.
   (the turn took 28 s, on CPU)
ok   the turn completed
ok   nothing was filed by the turn
   The page says: A ticket was proposed, not filed.
   alice opens /proposals/bb03536a-6255-425a-8165-02303c1f6421. It shows what will be filed, marked as the model's, and where:
   title: High-frequency restarts of web pods in hall 3
   priority: high
   body: Web pods in hall 3 are restarting frequently after deployment. Possible causes include memory or CPU resource exhaustion, leading to OOM (Out of Memory) kills or evictions. Investigate resource limits …
   repository: maintenance/tickets
   Sources shown with their text, read with her role: 3
ok   a confirmation without the session's CSRF token is refused
ok   and files nothing
ok   alice confirms
   ticket #3 was filed by the executor.
ok   exactly one ticket filed

== 5. Every decision is in the audit log
ok   the audit chain is unbroken
   52 rows. The last of this demo's decisions:
   propose  allowed  alice
   confirm  allowed  alice
   execute  allowed  alice
   result  succeeded  executor
   make demo now copies the log off the VM, where a later rewrite of these rows would show.

demo: every rule held
ok audit copy: 5 new rows, 52 in all, last hash 8cb33f2bca05cc9e23e1c88ccbaa7dbba98b554ad8b0e2ff4b48a71cb1583e04, in /mnt/data/airgapai/audit/gateway-audit-7711479058008002593.jsonl
```

The run took 2 min 40 s, and the four turns 28 to 42 s each.
