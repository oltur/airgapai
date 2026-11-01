GITLEAKS_VERSION := 8.30.1
export PATH := $(HOME)/.local/bin:$(PATH)

.DEFAULT_GOAL := help
.PHONY: help setup check test-secrets tools lint hours bundle vm-up vm-ssh vm-down test-hardening test-signed-images test-gitops test-egress rebuild

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

lint: ## Run ShellCheck, Ruff, go vet and govulncheck
	@shellcheck -x -P SCRIPTDIR -S warning $$(git ls-files '*.sh') .githooks/pre-commit .githooks/pre-push
	@ruff check --quiet .
	@for module in test/egress test/probe; do \
		go -C $$module vet ./... && (cd $$module && govulncheck ./...) || exit 1; \
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

test-egress: ## Test every egress path and the layer matrix from outside the cluster, with controls
	@go -C test/egress run . -root "$(CURDIR)"

rebuild: ## Destroy the VM, rebuild the bundle and the VM, and run every test
	@start=$$(date +%s); \
	$(MAKE) --no-print-directory vm-down && \
	$(MAKE) --no-print-directory bundle && \
	$(MAKE) --no-print-directory vm-up && \
	built=$$(date +%s) && \
	$(MAKE) --no-print-directory test-hardening && \
	$(MAKE) --no-print-directory test-signed-images && \
	$(MAKE) --no-print-directory test-gitops && \
	$(MAKE) --no-print-directory test-egress && \
	echo "rebuild passed: built in $$((built - start)) s, tested in $$(($$(date +%s) - built)) s"
