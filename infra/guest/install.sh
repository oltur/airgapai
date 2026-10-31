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
cp rke2-images.linux-amd64.tar.zst /var/lib/rancher/rke2/agent/images/
tar -xzf rke2.linux-amd64.tar.gz -C /usr/local
restorecon -R /usr/local/bin /usr/local/lib/systemd /var/lib/rancher 2>/dev/null || true

cat >/etc/rancher/rke2/config.yaml <<EOF
node-ip: $node_ip
selinux: true
disable-default-registry-endpoint: true
EOF
# Flannel normally finds its interface from the default route, which here is a black hole.
cat >/var/lib/rancher/rke2/server/manifests/rke2-canal-config.yaml <<EOF
apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata:
  name: rke2-canal
  namespace: kube-system
spec:
  valuesContent: |-
    flannel:
      iface: eth0
EOF

systemctl enable --now rke2-server
