# Candidate changes for the eval gates

Each file here is one deliberately changed part of the stack, built as a signed test image by
`infra/build-registry.sh` and run beside the deployed stack, never in place of it. Nothing in `deploy/`
names these images. A name is a registry tag: a-z, 0-9 and -. An edited file needs a rebuild before
it can be run; until then the run is refused, since each image is labelled with its file's hash.

- **`NAME.prompt`** is a system prompt for a candidate gateway: the deployed gateway's source with only
  its prompt replaced, as `gateway-candidate:NAME`. `test/eval/generation.py --candidate NAME` asks the
  golden set through a copy of the gateway running it. See ADR 0027.
- **`NAME.max-chars`** is a chunk size for a candidate ingest program: the deployed ingest source with
  only `maxChars` replaced, and the same pinned corpus, as `ingest-candidate:NAME`.
  `test/eval/retrieval.py --candidate NAME` fills a database of its own with it and searches the
  golden set through a copy of the retrieval service reading that database. See ADR 0031.

The candidates:

- `worse-prompt.prompt` is deliberately worse: a shorter prompt that drops the rules to answer only
  from passages, to cite them, to say when the manuals do not answer, and to treat passages as
  reference material. It is here to show that the generation gate fails it.
- `worse-chunking.max-chars` is deliberately worse: at most 200 characters of a page in each chunk,
  after its title, against 1500 deployed. A dry run over the corpus cut it into 3005 chunks, against
  507. It is here to show that the retrieval gate fails it.
