# 0004: Offline install from a read-only bundle, with a black-hole default route

**Status:** accepted
**Date:** 2026-10-31

## Context

The VM must install and run with no path out. Three layers were chosen: a libvirt network with no
forwarding, DNS or DHCP; a host nftables table that drops anything the VM starts; and a black-hole
default route in the guest, so a mistake in the first two still has no way out.

RKE2 and Calico assume a usable default route, so the black hole had a cost that only testing could
show.

## Decision

- **Bundle:** `make bundle` builds a read-only ISO on the online host, holding RPMs, the RKE2
  tarball and images, keys and checksums. RPMs are resolved in a short-lived VM from the same base
  image as the air-gapped VM, so the set matches that base.
- **Guest routes:** keep the black-hole default, and add the two specific routes it breaks.

| Route | Needed because |
|---|---|
| `10.43.0.0/16 dev eth0` | Host-network pods reach the API by service IP, which would match the black hole before kube-proxy translates it |
| `169.254.1.1/32 dev eth0` | Pods reach their gateway through proxy ARP, which the kernel answers only with a usable route to that address |

Flannel's interface is set to `eth0`, as it cannot find one from the default route.

- **Host firewall at boot:** a systemd unit loads a root-owned copy of the rules before libvirt
  starts. libvirt autostarts the network at boot, so rules loaded only by `make vm-up` left it
  without the firewall after a host restart, which a power failure showed.
- **Readiness:** `make vm-up` requires the node to be Ready and every install job to complete. Node
  Ready alone was reported while pod networking was broken.

## Alternatives

- **Default route to the host, dropped there:** simpler, needs neither specific route, and is the
  fallback if the black hole keeps costing time. It removes the guest-side layer, leaving a
  mistake in the host firewall one step closer to the outside.
- **Container as the RPM builder:** its package set differs from the cloud image, so dependencies
  could resolve differently.
- **Writable bundle disk:** read-only is enforced by the CD-ROM device, not only by a mount option.

## Consequences

Tested on 2026-10-31:

| Check | Result |
|---|---|
| Rebuild from the bundle with no route out | Node Ready, 8 of 8 install jobs complete |
| VM to public IP | Fails (exit 7) with the host firewall on and off |
| VM to host SSH | Timeout with the firewall on; reached with it off (control) |
| Online host to the same public IP | Reached (control) |
| Bundle write from the VM | Refused, read-only file system |
| Proxy ARP, from a pod's network namespace | Fails without the route, works with it (control) |
| Reboot | Routes, SELinux enforcing, node and jobs all return |

What this does not prove:

- **No formal egress test yet.** These are manual probes. The Go egress test, the layer matrix and
  the DNS canary are still to come, and the VM to public IP result has no open-path control.
- **Provenance is limited.** RPM signatures are checked on the online host, using the AlmaLinux key
  from the image and a Rancher key fetched over HTTPS. RKE2 artifacts are checked against the
  checksum file from the same release, not a signature. Inside the VM these checks show only that the
  bundle matches itself.
- **Not hardened yet.** CIS profile, Pod Security and the default-deny policy are not applied in this
  build. CoreDNS still has `forward`.
- **Firewall at boot is not yet tested by a host reboot.** The unit's order and enablement were
  checked, and reloading it restores the block, but the host has not been rebooted with it installed.
- **`10.43.0.0/16` is open to the wire.** An untranslated service IP goes out on `eth0` and depends on
  the host firewall to stop it.

## Measurements

Measured on 2026-10-31 on the NUC, in a VM limited to 8 GiB and 4 vCPUs, one run each, with the
default RKE2 components including Traefik and metrics-server:

| What | Value |
|---|---|
| `make vm-up`, including VM creation, until all install jobs complete | 119 s |
| Bundle ISO | 811 MiB |
| Guest memory in use just after install, excluding cache | 1805 MiB |
| `/var/lib/rancher/rke2` | 3.9 GiB |
