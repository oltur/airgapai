#!/usr/bin/env bash
# Opens a shell on the VM, or runs the given command there. The key and host key change on each
# make vm-up, so the host key is not recorded.
set -euo pipefail

source "$(cd "$(dirname "$0")" && pwd)/versions.env"
exec ssh -i "$DATA_DIR/vm/id_ed25519" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "alma@$VM_IP" "$@"
