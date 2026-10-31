GITLEAKS_VERSION := 8.30.1
export PATH := $(HOME)/.local/bin:$(PATH)

.DEFAULT_GOAL := help
.PHONY: help setup check test-secrets hours bundle vm-up vm-ssh vm-down test-hardening

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
