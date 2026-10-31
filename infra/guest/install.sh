#!/usr/bin/env bash
# Runs inside the VM as root, from the mounted bundle. Uses nothing but the bundle.
set -euo pipefail

bundle="$(cd "$(dirname "$0")" && pwd)"
node_ip="$1"
cd "$bundle"
sha256sum --quiet -c SHA256SUMS

rpm --import rpms/rancher.key rpms/RPM-GPG-KEY-AlmaLinux-9
rpm -K --quiet rpms/*.rpm
dnf -q -y --disablerepo='*' localinstall rpms/*.rpm

mkdir -p /var/lib/rancher/rke2/agent/images /var/lib/rancher/rke2/server/manifests /etc/rancher/rke2
cp rke2-images.linux-amd64.tar.zst probe.tar /var/lib/rancher/rke2/agent/images/
tar -xzf rke2.linux-amd64.tar.gz -C /usr/local

# Prerequisites of the CIS profile
getent passwd etcd >/dev/null || useradd -r -c "etcd user" -s /sbin/nologin -M -U etcd
cp /usr/local/share/rke2/rke2-cis-sysctl.conf /etc/sysctl.d/60-rke2-cis.conf
systemctl restart systemd-sysctl

cp config/pod-security.yaml /etc/rancher/rke2/pod-security.yaml
cp manifests/*.yaml /var/lib/rancher/rke2/server/manifests/
restorecon -R /usr/local/bin /usr/local/lib/systemd /var/lib/rancher /etc/rancher 2>/dev/null || true

cat >/etc/rancher/rke2/config.yaml <<EOF
node-ip: $node_ip
selinux: true
disable-default-registry-endpoint: true
profile: cis
pod-security-admission-config-file: /etc/rancher/rke2/pod-security.yaml
disable:
  - rke2-traefik
  - rke2-metrics-server
EOF
systemctl enable --now rke2-server
