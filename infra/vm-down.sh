#!/usr/bin/env bash
# Removes the VM and its generated files. Keeps the bundle, the downloads and the network.
set -euo pipefail

source "$(cd "$(dirname "$0")" && pwd)/versions.env"
export LIBVIRT_DEFAULT_URI=qemu:///system
virsh destroy "$VM_NAME" 2>/dev/null || true
virsh undefine "$VM_NAME" --nvram 2>/dev/null || true
sudo rm -rf "$DATA_DIR/vm"
