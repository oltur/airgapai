# Candidate changes for the generation gate

Each `NAME.prompt` here is a system prompt for a candidate gateway: the deployed gateway's source with
only its prompt replaced. `infra/build-registry.sh` builds each one as the signed test image
`gateway-candidate:NAME`, labelled with the prompt's hash, and `test/eval/generation.py --candidate NAME` asks the golden set through a
copy of the gateway running it, against the saved baseline. Nothing in `deploy/` names these images. A name is a registry tag: a-z, 0-9 and -. An edited prompt
needs a rebuild before it can be run; until then the run is refused.

- `worse-prompt.prompt` is deliberately worse: a shorter prompt that drops the rules to answer only
  from passages, to cite them, to say when the manuals do not answer, and to treat passages as
  reference material. It is here to show that the gate fails it. See ADR 0027.
