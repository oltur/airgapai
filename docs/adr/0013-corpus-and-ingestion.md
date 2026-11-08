# 0013: Corpus and ingestion

**Status:** accepted
**Date:** 2026-11-08

## Context

Retrieval needs a corpus in `rag.chunks`, from ADR 0012. The plan names the source and the method:
Kubernetes documentation first, Markdown only, ingested by an idempotent Go Job. Inside the air gap,
the corpus has to arrive in the bundle, as the images do, and be checked the same way.

Embedding is the slow step. ADR 0011 measured 1.92 and 2.26 chunks of 397 tokens per second in the VM,
so the corpus size sets how long every rebuild takes.

## Decision

- **Corpus: four directories of the Kubernetes concepts documentation,** `security`, `policy`,
  `configuration` and `services-networking`, from `github.com/kubernetes/website` at commit
  `a79720f`: 42 Markdown files. The documentation is under CC BY 4.0, per the repository's licence
  record. The image carries a notice naming the source, commit, authors and licence, and every chunk
  keeps its file path, so an answer can cite where it came from.
- **Pinned file by file.** `images/ingest/corpus.lock` holds the commit and each file's SHA-256, written
  by `infra/lock-corpus.sh`. The bundle build fetches exactly those files and fails on any hash that
  differs.
- **The corpus is baked into the ingestion image,** next to the `ingest` program, on an empty base with
  fixed owners, modes and times, like the model images in ADR 0011. Two builds gave the same digest, so
  `infra/versions.env` and `deploy/retrieval` pin it, and the signature policy checks it.
- **Synthetic labels by path:** chunks from `security` are `restricted`, and the rest are `public`. The
  rule is in the Job's arguments in Git. It is a test of the mechanism, and the documents themselves are
  public.
- **Chunks:** front matter is removed, the page title starts each chunk, and Hugo shortcodes become the
  text a reader would see, or nothing. A heading starts a new chunk once the current one has 400
  characters, and no chunk body passes 1500, which keeps code well inside the embedding model's
  2048-token window. Code blocks are not cut unless they are longer than that.
- **Idempotent by content hash.** A chunk is keyed by source, path and position. A chunk whose text and
  label are unchanged is not embedded or written. A changed label alone is updated without a new
  embedding. Rows for chunks or files that no longer exist are deleted. Each file is written in its own
  transaction, and an advisory lock stops two runs from interleaving.
- **Nomic's document prefix,** `search_document: `, is added before embedding. Queries must use
  `search_query: `.
- **A Job from Git,** replaced by Argo CD when its file changes, because a Job's pod template cannot be
  edited in place.
- **Network: the Job reaches PostgreSQL on 5432 and the embedding server on 8080, and nothing else
  reaches either.** The rules are Calico policies in `infra/guest/manifests`, beside the other allow
  rules, and select pods by the label `app=ingest`.
- **Plain TCP to PostgreSQL,** with a SCRAM password. The server has no certificate yet.

## Alternatives

- **The whole documentation, or the whole concepts section.** The concepts section alone is about
  2.3 MB of Markdown, against 660 KB here, and would add more than ten minutes of embedding to each
  rebuild at the measured speed, by arithmetic. Four directories are enough to show that the same
  question under two roles retrieves different chunks.
- **A tarball of the repository at the commit.** One hash is simpler, but the tarball holds every
  language and all the site's images, and the bundle would carry it all.
- **Mount the corpus from a volume or a ConfigMap.** The signature policy would not cover it, and a
  ConfigMap is limited to 1 MiB.
- **Labels in the front matter or a separate file.** No upstream file carries one, so a path rule is the
  smallest thing that works, and it is visible in Git.
- **Re-embed everything on each run.** Simpler, but a rerun would then take minutes and could not show
  that nothing changed.

## Consequences

- **A corpus change is a lock change,** which changes the image digest, so `versions.env` and
  `deploy/retrieval` change with it. A program change does the same. The bundle build names any
  mismatch.
- **Anything in the `retrieval` namespace that carries `app=ingest` gets the Job's network paths,** and
  with the Secret, its password. Calico selects by label, and labels are not protected. The test uses
  this to check the rules, and the gateway's threat model must list it.
- **The ingest login can write any row with any label.** Row-level security filters readers, not the
  writer. The labels are only as right as the Job.
- **Ingestion and answering compete for the CPU,** as ADR 0011 measured. The Job runs once after each
  rebuild and again only when Git changes it.
- **Shortcodes are cleaned by rules written for these 42 files.** Another directory may use shortcodes
  the rules do not know. Those are removed, but their text is lost, and `make test-ingest` checks only
  that no shortcode markup is left.
- **The chunk count is not the token count.** Chunks of up to 1500 characters stay inside the window
  here, and an oversized chunk fails the run loudly rather than being cut by the server.

### What this does not prove

- **That the chunks are good for retrieval.** Recall is measured with the retrieval service.
- **That the label rule fits the content.** The rule labels a directory, not what a page says.
- **That only the Job uses its network paths.** See the label above.
- **That the source is authentic beyond the hashes.** The hashes pin the files as first fetched from
  GitHub, which is not a signature from the Kubernetes project.
- **Licence advice.** The licence was read from the repository's record, which is not legal advice.

## Measurements

On 2026-11-08 on the host, before the cluster, against a PostgreSQL container started from the same
image and schema, and the Nomic model under llama.cpp `b12032` with the cluster's settings:

| Run | Result |
|---|---|
| Dry run over the corpus | 42 files: 23 public with 314 chunks, 19 restricted with 193 chunks. Largest chunk 1533 characters, including its title |
| First run, empty table | 507 inserted in 119.6 s |
| Second run | 507 unchanged, nothing written, 0.1 s |
| Every label removed, then restored | 193 relabelled each time, no new embeddings, 0.2 s |
| One file removed and one shortened | 13 deleted and 1 updated; restoring the corpus inserted the 13 again |

The first run used an earlier version of the chunker, which left shortcodes that span lines. A scan of
every chunk found them, and the fixed chunker gives the same 507 chunks with none left. Its first run
was not timed on the host.

In the VM, on 2026-11-08, `make rebuild` passed every suite, including every check of `make test-ingest`:
built in 251 s and tested in 690 s, against 539 s for the rebuild before ingestion existed. The Job from
Git inserted 507 chunks, 314 public and 193 restricted. Argo CD's recreated Job wrote nothing, and the rows,
ids and embeddings hashed the same before and after. The Job was not replaced in the minute after. The
first run's own duration was not recorded, because the test dropped it from its output; the test now
prints it. No out-of-memory kills were reported, and afterwards the guest had 4335 MiB available.
