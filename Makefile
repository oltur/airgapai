GITLEAKS_VERSION := 8.30.1
export PATH := $(HOME)/.local/bin:$(PATH)

.DEFAULT_GOAL := help
.PHONY: help setup check test-secrets tools lint hours bundle vm-up vm-ssh vm-down test-hardening test-signed-images test-gitops test-egress test-rls test-ingest test-retrieval measure-inference measure-retrieval rebuild

help: ## Show this help
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "%-14s %s\n", $$1, $$2}'

setup: ## Check gitleaks and the denylist, then enable the git hooks
	@test "$$(gitleaks version 2>/dev/null)" = "$(GITLEAKS_VERSION)" || \
		{ echo "gitleaks $(GITLEAKS_VERSION) is required in ~/.local/bin"; exit 1; }
	@test -s .denylist || { echo "create .denylist first, one extended regex per line"; exit 1; }
	@chmod 600 .denylist
	@git config core.hooksPath .githooks
	@echo "hooks enabled"

check: ## Scan the full git history for secrets and denylisted terms
	@scripts/guard.sh history

test-secrets: ## Prove the guard blocks fake secrets in a temporary repo
	@scripts/test-secrets.sh

tools: ## Install the pinned guard and lint tools into ~/.local/bin
	@scripts/install-tools.sh

lint: ## Run ShellCheck, Ruff, go vet, go test and govulncheck
	@for tool in shellcheck ruff go govulncheck; do \
		command -v $$tool >/dev/null || { echo "$$tool not found, see make tools"; exit 1; }; \
	done
	@shellcheck -x -P SCRIPTDIR -S warning $$(git ls-files '*.sh' '.githooks/*')
	@ruff check --quiet .
	@for module in $$(git ls-files '*/go.mod' | xargs -n1 dirname); do \
		go -C $$module vet ./... || exit 1; \
		go -C $$module test -count=1 ./... >/dev/null || exit 1; \
		(cd $$module && govulncheck ./...) || exit 1; # Its -C flag would scan the wrong toolchain. \
	done
	@echo "lint clean"

hours: ## Regenerate docs/hours.md from session and commit activity
	@scripts/hours.py

bundle: ## Build the offline bundle on the online host
	@infra/build-bundle.sh

vm-up: ## Create the isolated network and VM, and install RKE2 from the bundle
	@infra/vm-up.sh

vm-ssh: ## Open a shell on the VM
	@infra/vm-ssh.sh

vm-down: ## Remove the VM
	@infra/vm-down.sh

test-hardening: ## Test Pod Security, default deny and DNS on the VM, with controls
	@infra/vm-ssh.sh sudo bash -s <test/hardening.sh

test-signed-images: ## Test signed-image admission and the read-only registry, with controls
	@infra/vm-ssh.sh sudo bash -s <test/signed-images.sh

test-gitops: ## Test that Git drives the cluster and CI catches a secret, with controls
	@infra/vm-ssh.sh sudo bash -s <test/gitops.sh

test-rls: ## Test row-level security on the chunks table, with controls
	@infra/vm-ssh.sh sudo bash -s <test/rls.sh

test-ingest: ## Test the ingestion Job, a second run, the labels and its network paths, with controls
	@infra/vm-ssh.sh sudo bash -s <test/ingest.sh

test-retrieval: ## Test the retrieval service's roles, a leak test and its network paths, with controls
	@infra/vm-ssh.sh sudo QUERIES="$$(base64 -w0 test/retrieval/queries.txt)" bash -s <test/retrieval.sh

test-egress: ## Test every egress path and the layer matrix from outside the cluster, with controls
	@go -C test/egress run . -root "$(CURDIR)"

measure-inference: ## Restart the model pods in the VM and measure start-up, memory, speed and tool calls
	@test/inference/measure.py --out test/inference/results

measure-retrieval: ## Measure recall per role against an exact scan, and search latency, in the VM
	@test/retrieval/measure.py --out test/retrieval/results

rebuild: ## Destroy the VM, rebuild the bundle and the VM, and run every test
	@start=$$(date +%s); \
	$(MAKE) --no-print-directory vm-down && \
	$(MAKE) --no-print-directory bundle && \
	$(MAKE) --no-print-directory vm-up && \
	built=$$(date +%s) && \
	$(MAKE) --no-print-directory test-hardening && \
	$(MAKE) --no-print-directory test-signed-images && \
	$(MAKE) --no-print-directory test-gitops && \
	$(MAKE) --no-print-directory test-rls && \
	$(MAKE) --no-print-directory test-ingest && \
	$(MAKE) --no-print-directory test-retrieval && \
	$(MAKE) --no-print-directory test-egress && \
	echo "rebuild passed: built in $$((built - start)) s, tested in $$(($$(date +%s) - built)) s"
