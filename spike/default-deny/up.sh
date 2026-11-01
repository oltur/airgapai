#!/usr/bin/env bash
# Throwaway VM with single-node RKE2 on libvirt's NAT network, for the default-deny spike only.
# Not the air-gapped build: this VM has internet access, so the test can show a pod reaching it.
set -euo pipefail

export LIBVIRT_DEFAULT_URI=qemu:///system
name=airgapai-spike
dir=/mnt/data/airgapai/spike
image=AlmaLinux-9-GenericCloud-9.8-20260810.x86_64.qcow2
image_sha256=6bdab6376d46d42e4203ace3733efafc7c5d37c7cb443a6cc74750097002d74b
rke2_version=v1.36.5+rke2r1

sudo mkdir -p "$dir"
sudo chown "$(id -un)" "$dir"
cd "$dir"

[[ -f $image ]] || curl -fsSL -o "$image" "https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/$image"
echo "$image_sha256  $image" | sha256sum -c -
[[ -f id_ed25519 ]] || ssh-keygen -q -t ed25519 -N '' -f id_ed25519

if ! virsh dominfo "$name" >/dev/null 2>&1; then
  qemu-img create -q -f qcow2 -F qcow2 -b "$dir/$image" disk.qcow2 40G
  cat >user-data <<EOF
#cloud-config
users:
  - name: alma
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - $(cat id_ed25519.pub)
EOF
  printf 'instance-id: %s\nlocal-hostname: %s\n' "$name" "$name" >meta-data
  xorriso -as mkisofs -quiet -o seed.iso -volid cidata -joliet -rock user-data meta-data
  virt-install --name "$name" --memory 6144 --vcpus 4 --osinfo linux2022 \
    --disk "$dir/disk.qcow2,bus=virtio" --disk "$dir/seed.iso,device=cdrom" --import \
    --network network=default,model=virtio --serial "file,path=$dir/console.log" \
    --graphics none --noautoconsole
fi

echo "waiting for an address"
for _ in $(seq 60); do
  ip="$(virsh domifaddr "$name" 2>/dev/null | awk '/ipv4/ {sub("/.*","",$4); print $4; exit}')"
  [[ -n $ip ]] && break
  sleep 5
done
[[ -n $ip ]] || { echo "no address"; exit 1; }
echo "$ip" >ip
ssh_vm() { ssh -i "$dir/id_ed25519" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "alma@$ip" "$@"; }

for _ in $(seq 60); do ssh_vm true 2>/dev/null && break; sleep 5; done
ssh_vm 'cloud-init status --wait >/dev/null; getenforce'

ssh_vm sudo bash -s -- "$rke2_version" <<'EOF'
set -euo pipefail
systemctl disable --now firewalld 2>/dev/null || true
mkdir -p /etc/NetworkManager/conf.d /etc/rancher/rke2
cat >/etc/NetworkManager/conf.d/rke2-canal.conf <<CONF
[keyfile]
unmanaged-devices=interface-name:cali*;interface-name:flannel*
CONF
systemctl reload NetworkManager
curl -fsSL https://get.rke2.io | INSTALL_RKE2_VERSION="$1" INSTALL_RKE2_METHOD=rpm sh -
systemctl enable --now rke2-server
for _ in $(seq 120); do
  /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml get nodes 2>/dev/null | grep -q ' Ready ' && break
  sleep 5
done
/var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml get nodes
getenforce
rpm -q rke2-selinux rke2-server
EOF
echo "up at $ip"
