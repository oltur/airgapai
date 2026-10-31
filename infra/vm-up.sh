#!/usr/bin/env bash
# Creates the isolated network, the host firewall and the VM, then installs RKE2 from the bundle.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
source "$here/versions.env"
source "$here/lock.sh"
export LIBVIRT_DEFAULT_URI=qemu:///system
vm="$DATA_DIR/vm"
iso="$DATA_DIR/bundle.iso"
[[ -f $iso ]] || { echo "no bundle, run make bundle"; exit 1; }
# The next steps delete the VM's disk, so stop here while it can still be kept.
if virsh dominfo "$VM_NAME" >/dev/null 2>&1; then
  echo "VM $VM_NAME already exists. Start it with virsh start, or run make vm-down first."
  exit 1
fi

sudo install -D -m 644 "$here/firewall.nft" /etc/airgapai/firewall.nft
sudo install -m 644 "$here/airgapai-firewall.service" /etc/systemd/system/airgapai-firewall.service
sudo systemctl daemon-reload
sudo systemctl enable airgapai-firewall.service
sudo systemctl restart airgapai-firewall.service
if ! virsh net-info airgapai >/dev/null 2>&1; then
  virsh net-define "$here/network.xml" >/dev/null
  virsh net-autostart airgapai >/dev/null
fi
[[ $(virsh net-info airgapai | awk '/^Active/ {print $2}') == yes ]] || virsh net-start airgapai >/dev/null

sudo rm -rf "$vm"
mkdir -p "$vm"
cd "$vm"
ssh-keygen -q -t ed25519 -N '' -f id_ed25519
qemu-img create -q -f qcow2 -F qcow2 -b "$DATA_DIR/downloads/$ALMA_IMAGE" disk.qcow2 60G
cat >user-data <<EOF
#cloud-config
users:
  - name: alma
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - $(cat id_ed25519.pub)
write_files:
  - path: /etc/profile.d/kubectl.sh
    content: |
      alias kubectl='sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml'
      alias k=kubectl
  - path: /etc/systemd/system/blackhole-default.service
    content: |
      [Unit]
      Description=Black-hole default route
      After=network-online.target
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/usr/sbin/ip route replace blackhole default
      # Host-network pods reach the API by service IP, which must not match the black hole before
      # kube-proxy translates it. Anything left untranslated ends at the host firewall.
      ExecStart=/usr/sbin/ip route replace 10.43.0.0/16 dev eth0
      # Pods reach their gateway through proxy ARP, which the kernel only answers if it has a
      # usable route to that address. No traffic is ever sent to it.
      ExecStart=/usr/sbin/ip route replace 169.254.1.1/32 dev eth0
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [systemctl, daemon-reload]
  - [systemctl, enable, --now, blackhole-default.service]
  - [systemctl, disable, --now, firewalld]
EOF
printf 'instance-id: %s\nlocal-hostname: %s\n' "$VM_NAME" "$VM_NAME" >meta-data
cat >network-config <<EOF
version: 2
ethernets:
  eth0:
    match:
      macaddress: "52:54:00:77:00:10"
    set-name: eth0
    addresses: [$VM_IP/24]
EOF
xorriso -as mkisofs -quiet -o seed.iso -volid cidata -joliet -rock user-data meta-data network-config

virt-install --name "$VM_NAME" --memory "$VM_MEMORY_MIB" --vcpus "$VM_VCPUS" --osinfo linux2022 \
  --disk "$vm/disk.qcow2,bus=virtio" --disk "$vm/seed.iso,device=cdrom" \
  --disk "$iso,device=cdrom,readonly=on" --import \
  --network network=airgapai,model=virtio,mac=52:54:00:77:00:10 \
  --serial "file,path=$vm/console.log" --graphics none --noautoconsole >/dev/null

# Keepalives make a dead VM fail this script instead of hanging it.
opts=(-i "$vm/id_ed25519" -o IdentitiesOnly=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)
for _ in $(seq 60); do ssh "${opts[@]}" "alma@$VM_IP" true 2>/dev/null && break; sleep 5; done
ssh "${opts[@]}" "alma@$VM_IP" sudo bash -s -- "$VM_IP" <<'EOF'
set -euo pipefail
cloud-init status --wait >/dev/null || true
mkdir -p /mnt/bundle
mount -o ro -L AIRGAPBUNDLE /mnt/bundle
bash /mnt/bundle/install.sh "$1"
ready=no
for _ in $(seq 120); do
  if /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml get nodes 2>/dev/null | grep -q ' Ready '; then
    ready=yes
    break
  fi
  sleep 5
done
/var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml get nodes
[[ $ready == yes ]] || { echo "node did not become Ready"; exit 1; }
# Ready alone is not enough: it was reported while pod networking was broken.
for _ in $(seq 60); do
  pending="$(/var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml get jobs -A --no-headers 2>/dev/null | awk '$3 != "Complete"' | wc -l)"
  jobs="$(/var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml get jobs -A --no-headers 2>/dev/null | wc -l)"
  [[ $jobs -gt 0 && $pending == 0 ]] && { echo "all $jobs install jobs complete"; exit 0; }
  sleep 5
done
echo "install jobs did not complete"
exit 1
EOF
