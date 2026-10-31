#!/usr/bin/env bash
# Runs on the online host. Builds the read-only bundle ISO the air-gapped VM installs from.
# RPMs are resolved in a short-lived VM from the same base image, so the set matches that base.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
source "$here/versions.env"
export LIBVIRT_DEFAULT_URI=qemu:///system
builder=airgapai-builder
dl="$DATA_DIR/downloads"
work="$DATA_DIR/builder"
tree="$DATA_DIR/bundle-tree"
iso="$DATA_DIR/bundle.iso"

sudo mkdir -p "$DATA_DIR"
sudo chown "$(id -un)" "$DATA_DIR"
mkdir -p "$dl"
sudo rm -rf "$work" "$tree"
mkdir -p "$work" "$tree/rpms"
cd "$dl"

[[ -f $ALMA_IMAGE ]] || curl -fsSL -o "$ALMA_IMAGE" "https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/$ALMA_IMAGE"
echo "$ALMA_IMAGE_SHA256  $ALMA_IMAGE" | sha256sum -c -

base="https://github.com/rancher/rke2/releases/download/${RKE2_VERSION/+/%2B}"
for file in sha256sum-amd64.txt rke2.linux-amd64.tar.gz rke2-images.linux-amd64.tar.zst; do
  [[ -f $file ]] || curl -fsSL -o "$file" "$base/$file"
done
grep -E ' (rke2\.linux-amd64\.tar\.gz|rke2-images\.linux-amd64\.tar\.zst)$' sha256sum-amd64.txt | sha256sum -c -

cd "$work"
ssh-keygen -q -t ed25519 -N '' -f id_ed25519
qemu-img create -q -f qcow2 -F qcow2 -b "$dl/$ALMA_IMAGE" disk.qcow2 20G
cat >user-data <<EOF
#cloud-config
users:
  - name: alma
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - $(cat id_ed25519.pub)
EOF
printf 'instance-id: %s\nlocal-hostname: %s\n' "$builder" "$builder" >meta-data
xorriso -as mkisofs -quiet -o seed.iso -volid cidata -joliet -rock user-data meta-data

cleanup() { virsh destroy "$builder" >/dev/null 2>&1 || true; virsh undefine "$builder" --nvram >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
virt-install --name "$builder" --memory 2048 --vcpus 2 --osinfo linux2022 \
  --disk "$work/disk.qcow2,bus=virtio" --disk "$work/seed.iso,device=cdrom" --import \
  --network network=default,model=virtio --graphics none --noautoconsole >/dev/null

ip=""
for _ in $(seq 60); do
  ip="$(virsh domifaddr "$builder" 2>/dev/null | awk '/ipv4/ {sub("/.*","",$4); print $4; exit}')"
  [[ -n $ip ]] && break
  sleep 5
done
[[ -n $ip ]] || { echo "builder got no address"; exit 1; }
opts=(-i "$work/id_ed25519" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)
for _ in $(seq 60); do ssh "${opts[@]}" "alma@$ip" true 2>/dev/null && break; sleep 5; done

ssh "${opts[@]}" "alma@$ip" sudo bash -s <<'EOF'
set -euo pipefail
cloud-init status --wait >/dev/null
cat >/etc/yum.repos.d/rke2-common.repo <<REPO
[rke2-common]
name=RKE2 common
baseurl=https://rpm.rancher.io/rke2/stable/common/centos/9/noarch
enabled=1
gpgcheck=1
repo_gpgcheck=0
gpgkey=https://rpm.rancher.io/public.key
REPO
dnf -q -y install dnf-plugins-core >/dev/null
mkdir -p /tmp/rpms
dnf -q -y download --resolve --destdir /tmp/rpms rke2-selinux container-selinux iptables-nft tar zstd
curl -fsSL -o /tmp/rancher.key https://rpm.rancher.io/public.key
rpm --import /tmp/rancher.key /etc/pki/rpm-gpg/RPM-GPG-KEY-AlmaLinux-9
rpm -K --quiet /tmp/rpms/*.rpm
cp /tmp/rancher.key /etc/pki/rpm-gpg/RPM-GPG-KEY-AlmaLinux-9 /tmp/rpms/
chmod -R a+rX /tmp/rpms
ls /tmp/rpms | wc -l
EOF
scp -q "${opts[@]}" "alma@$ip:/tmp/rpms/*" "$tree/rpms/"

cp "$here/guest/install.sh" "$tree/"
cp "$dl/rke2.linux-amd64.tar.gz" "$dl/rke2-images.linux-amd64.tar.zst" "$tree/"
(cd "$tree" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum >SHA256SUMS)
rm -f "$iso"
xorriso -as mkisofs -quiet -iso-level 3 -o "$iso" -volid AIRGAPBUNDLE -joliet -rock "$tree"
du -h "$iso"
sudo rm -rf "$work"
