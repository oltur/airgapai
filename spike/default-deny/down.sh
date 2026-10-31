#!/usr/bin/env bash
# Removes the spike VM and its files. Touches nothing else on the host.
set -euo pipefail

export LIBVIRT_DEFAULT_URI=qemu:///system
virsh destroy airgapai-spike 2>/dev/null || true
virsh undefine airgapai-spike --nvram 2>/dev/null || true
sudo rm -rf /mnt/data/airgapai/spike
