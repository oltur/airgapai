// Egress tests the air gap from outside the cluster, because it must change the host firewall and the
// VM's routes as its controls. A block only counts if the same attempt succeeds once the layer that
// blocked it is removed, so each layer is opened in turn, towards addresses that exist only for this
// test. No step opens a path to the internet. Everything it opens is closed on exit, on a signal and
// at the start of the next run.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	vmIP       = "10.77.0.10"
	hostIP     = "10.77.0.1"
	registry   = "10.43.0.50:5000"
	canaryIP   = "198.51.100.1" // TEST-NET-2: routed nowhere, so nothing real can answer for it
	metadataIP = "169.254.169.254"
	canaryLink = "airgap-canary"
	canaryTCP  = "18443"
	canaryDNS  = "18053"
	bridgeTCP  = "18444" // on the host's bridge address, behind the host firewall
	kubectl    = "/var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml "
	crictl     = "CRI_CONFIG_FILE=/var/lib/rancher/rke2/agent/etc/crictl.yaml /var/lib/rancher/rke2/bin/crictl "
	policyFile = "/var/lib/rancher/rke2/server/manifests/default-deny.yaml"
	policyName = "globalnetworkpolicies.crd.projectcalico.org/default-deny-workloads"
	settle     = 8 * time.Second // long enough for a policy change to reach the dataplane
)

var (
	root     string
	failures int
	testNS   = []string{"egress-new", "egress-zot", "egress-outage"}
	coreDNS  = "configmap/rke2-coredns-rke2-coredns"
)

func section(title string) { fmt.Printf("\n== %s\n", title) }
func info(format string, args ...any) {
	fmt.Printf("info %s\n", fmt.Sprintf(format, args...))
}

func check(label, want, got string) {
	if want == got {
		fmt.Printf("ok   %s: %s\n", label, got)
		return
	}
	fmt.Printf("FAIL %s: wanted %s, got %s\n", label, want, got)
	failures++
}

// A blocked attempt must fail without any answer. "refused" and "error" are not blocks.
func checkBlocked(label, got string) {
	if slices.Contains([]string{"timeout", "no-route", "dns-failed"}, got) {
		fmt.Printf("ok   %s: blocked (%s)\n", label, got)
		return
	}
	fmt.Printf("FAIL %s: wanted blocked, got %s\n", label, got)
	failures++
}

func fatal(format string, args ...any) {
	fmt.Printf("FAIL %s\n", fmt.Sprintf(format, args...))
	cleanup()
	os.Exit(1)
}

// vm runs a command as root on the VM and returns stdout. stderr is included only in the error.
func vm(stdin, command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, root+"/infra/vm-ssh.sh", "sudo bash -c "+quote(command))
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func kube(args string) (string, error) { return vm("", kubectl+args) }

func mustKube(args string) string {
	out, err := kube(args)
	if err != nil {
		fatal("kubectl %s: %v", args, err)
	}
	return out
}

func host(args ...string) error {
	out, err := exec.Command("sudo", append([]string{"-n"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

func mustHost(args ...string) {
	if err := host(args...); err != nil {
		fatal("%v", err)
	}
}

// probe runs the probe binary in a pod and returns its one-word result.
func probe(ns, pod string, args ...string) string {
	out, err := kube(fmt.Sprintf("-n %s exec %s -- /probe %s", ns, pod, strings.Join(args, " ")))
	if err != nil {
		return "exec-failed"
	}
	return out
}

// probeUntil allows for a policy or route change still reaching the dataplane.
func probeUntil(want, ns, pod string, args ...string) string {
	var got string
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		if got = probe(ns, pod, args...); got == want {
			break
		}
	}
	return got
}

// fromNode connects from the VM itself, outside any pod.
func fromNode(addr string) string {
	h, p, _ := net.SplitHostPort(addr)
	out, err := vm("", fmt.Sprintf("timeout 6 bash -c '</dev/tcp/%s/%s' 2>&1; echo rc=$?", h, p))
	if err != nil {
		return "ssh-failed"
	}
	switch {
	case strings.Contains(out, "rc=0"):
		return "reached"
	case strings.Contains(out, "rc=124"):
		return "timeout"
	case strings.Contains(out, "refused"):
		return "refused"
	}
	return "no-route"
}

func fromHost(addr string) string {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "failed"
	}
	conn.Close()
	return "reached"
}

// canary logs what reaches it from outside the cluster.
type canary struct {
	mu      sync.Mutex
	tcp     map[string]int
	queries []string
	closers []func() error
}

var theCanary *canary

func startCanary() *canary {
	c := &canary{tcp: map[string]int{}}
	for _, addr := range []string{canaryIP + ":" + canaryTCP, metadataIP + ":" + canaryTCP, hostIP + ":" + bridgeTCP} {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			fatal("canary listener: %v", err)
		}
		c.closers = append(c.closers, listener.Close)
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				c.mu.Lock()
				c.tcp[addr]++
				c.mu.Unlock()
				conn.Close()
			}
		}()
	}
	udp, err := net.ListenPacket("udp4", canaryIP+":"+canaryDNS)
	if err != nil {
		fatal("canary DNS: %v", err)
	}
	c.closers = append(c.closers, udp.Close)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			name := questionName(buf[:n])
			c.mu.Lock()
			c.queries = append(c.queries, name)
			c.mu.Unlock()
			if n >= 12 { // answer NXDOMAIN, so a forwarding resolver does not retry
				reply := slices.Clone(buf[:n])
				reply[2], reply[3] = 0x81, 0x83
				udp.WriteTo(reply, from)
			}
		}
	}()
	return c
}

func questionName(msg []byte) string {
	var labels []string
	for i := 12; i < len(msg) && msg[i] != 0; i += int(msg[i]) + 1 {
		end := i + 1 + int(msg[i])
		if end > len(msg) {
			break
		}
		labels = append(labels, string(msg[i+1:end]))
	}
	return strings.ToLower(strings.Join(labels, "."))
}

func (c *canary) tcpCount(addr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tcp[addr]
}

func (c *canary) logged(label string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, q := range c.queries {
		if strings.HasPrefix(q, label+".") {
			count++
		}
	}
	if count == 0 {
		return "not-logged"
	}
	return "logged"
}

func uniqueLabel() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "c" + hex.EncodeToString(b)
}

func openVMPath() {
	mustHost("nft", "insert", "rule", "inet", "airgapai", "input", "iifname", "virbr-airgap", "ip", "daddr",
		"{", canaryIP+",", metadataIP, "}", "accept")
	if _, err := vm("", "ip route replace "+canaryIP+"/32 via "+hostIP+" dev eth0 && ip route replace "+metadataIP+"/32 via "+hostIP+" dev eth0"); err != nil {
		fatal("open VM route: %v", err)
	}
}

func closeVMPath() {
	mustHost("nft", "-f", "/etc/airgapai/firewall.nft")
	if _, err := vm("", "ip route del "+canaryIP+"/32 2>/dev/null; ip route del "+metadataIP+"/32 2>/dev/null; true"); err != nil {
		fatal("close VM route: %v", err)
	}
}

func policyOff() { mustKube("delete " + policyName) }
func policyOn()  { mustKube("apply -f " + policyFile) }

func corefile() string {
	return mustKube("-n kube-system get " + coreDNS + " -o jsonpath={.data.Corefile}")
}

func setCorefile(content string) {
	if err := patchCorefile(content); err != nil {
		fatal("%v", err)
	}
}

// patchCorefile returns its error, because cleanup calls it and fatal calls cleanup.
func patchCorefile(content string) error {
	patch, err := json.Marshal(map[string]map[string]string{"data": {"Corefile": content}})
	if err != nil {
		return err
	}
	if _, err := vm(string(patch), kubectl+"-n kube-system patch "+coreDNS+" --type merge --patch-file /dev/stdin"); err != nil {
		return fmt.Errorf("patch CoreDNS: %w", err)
	}
	if _, err := kube("-n kube-system rollout restart deployment/rke2-coredns-rke2-coredns"); err != nil {
		return fmt.Errorf("restart CoreDNS: %w", err)
	}
	if _, err := kube("-n kube-system rollout status deployment/rke2-coredns-rke2-coredns --timeout=180s"); err != nil {
		return fmt.Errorf("wait for CoreDNS: %w", err)
	}
	return nil
}

var (
	forwardLine = regexp.MustCompile(`(?m)^.*forward .*\n`)
	cacheLine   = regexp.MustCompile(`(?m)^\s*cache\s.*$`)
)

var cleaned sync.Mutex

// cleanup puts every layer back. It runs after a crash too, so it assumes nothing about what was opened.
func cleanup() {
	cleaned.Lock()
	defer cleaned.Unlock()
	if theCanary != nil {
		for _, closer := range theCanary.closers {
			closer()
		}
		theCanary = nil
	}
	if err := host("nft", "-f", "/etc/airgapai/firewall.nft"); err != nil {
		fmt.Printf("FAIL restoring the host firewall: %v\n", err)
		failures++
	}
	host("ip", "link", "del", canaryLink) // absent on a clean run
	steps := []string{
		"ip route del " + canaryIP + "/32 2>/dev/null; ip route del " + metadataIP + "/32 2>/dev/null; true",
		kubectl + "apply -f " + policyFile,
		kubectl + "delete globalnetworkpolicies.crd.projectcalico.org egress-test-zot-client egress-test-zot-server --ignore-not-found",
		kubectl + "-n zot scale deployment zot --replicas=1",
		kubectl + "-n zot rollout status deployment/zot --timeout=180s",
		kubectl + "delete pod -n workloads egress-probe --ignore-not-found --wait=false",
		kubectl + "delete pod -n forgejo egress-probe --ignore-not-found --wait=false",
		kubectl + "delete pod -n argocd egress-probe --ignore-not-found --wait=false",
		kubectl + "delete pod -n kyverno egress-probe --ignore-not-found --wait=false",
		kubectl + "delete pod -n zot egress-probe --ignore-not-found --wait=false",
		kubectl + "delete pod -n kube-system egress-probe --ignore-not-found --wait=false",
		kubectl + "delete ns " + strings.Join(testNS, " ") + " --ignore-not-found --wait=false",
	}
	for _, step := range steps {
		if _, err := vm("", step); err != nil {
			fmt.Printf("FAIL cleanup step %q: %v\n", step, err)
			failures++
		}
	}
	if file, err := kube("-n kube-system get " + coreDNS + " -o jsonpath={.data.Corefile}"); err == nil && strings.Contains(file, "forward ") {
		if err := patchCorefile(forwardLine.ReplaceAllString(file, "")); err != nil {
			fmt.Printf("FAIL restoring CoreDNS: %v\n", err)
			failures++
		}
	}
}

func podYAML(ns, name, image string, hostNetwork bool) string {
	network := ""
	if hostNetwork {
		network = "  hostNetwork: true\n"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s, namespace: %s}
spec:
%s  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: probe
      image: %s
      args: [serve, ":8080"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
`, name, ns, network, image)
}

func apply(yaml string, extra string) (string, error) {
	return vm(yaml, kubectl+"apply "+extra+" -f - 2>&1")
}

// admission reports a rejection only when it comes from the named control, not from a mistake here.
func admission(yaml, marker string) string {
	out, err := apply(yaml, "--dry-run=server")
	switch {
	case err == nil:
		return "admitted"
	case strings.Contains(out, marker):
		return "rejected"
	}
	return "other: " + out
}

func parallel(count int, run func(int)) {
	var wg sync.WaitGroup
	limit := make(chan struct{}, 6)
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			run(i)
		}()
	}
	wg.Wait()
}

type target struct{ label, addr string }

// hostListeners finds what the host really serves, so a new service is covered without editing this.
func hostListeners() []target {
	out, err := exec.Command("ss", "-ltnH").Output()
	if err != nil {
		fatal("ss: %v", err)
	}
	seen := map[string]bool{}
	var found []target
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		local := fields[3]
		at := strings.LastIndex(local, ":")
		addr, port := local[:at], local[at+1:]
		var tgt target
		switch {
		case addr == "0.0.0.0" || addr == "*" || addr == "[::]":
			tgt = target{"host bridge address, port " + port, hostIP + ":" + port}
		case strings.HasPrefix(addr, "127.") || strings.HasPrefix(addr, "[") || strings.HasPrefix(addr, hostIP):
			continue
		default: // other host addresses stay out of the output
			tgt = target{"another host address, port " + port, addr + ":" + port}
		}
		if !seen[tgt.addr] {
			seen[tgt.addr] = true
			found = append(found, tgt)
		}
	}
	return found
}

func imageDigest(repo string) string {
	out, err := vm("", "curl -sI -H 'Accept: application/vnd.oci.image.manifest.v1+json' http://"+registry+"/v2/"+repo+"/manifests/v1")
	if err != nil {
		fatal("digest of %s: %v", repo, err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "docker-content-digest") {
			return registry + "/" + repo + "@" + strings.TrimSpace(value)
		}
	}
	fatal("no digest for %s", repo)
	return ""
}

func publicChecks(pods [][2]string, listeners []target) {
	gatewayTargets := append([]target{
		{"gateway", "169.254.1.1:80"},
		{"metadata address", metadataIP + ":80"},
		{"host SSH on the bridge", hostIP + ":22"},
		{"node API server", vmIP + ":6443"},
		{"node kubelet", vmIP + ":10250"},
		{"node SSH", vmIP + ":22"},
		{"node RKE2 supervisor", vmIP + ":9345"},
		{"API service", "10.43.0.1:443"},
	}, listeners...)
	publicTargets := []target{{"public IP", "1.1.1.1:443"}, {"second public IP", "8.8.8.8:53"}, {"HTTPS by name", "example.com:443"}, {"public IPv6", "[2606:4700:4700::1111]:443"}}

	for _, p := range pods {
		ns, pod := p[0], p[1]
		section("namespace " + ns + ": default deny on")
		all := append(slices.Clone(publicTargets), gatewayTargets...)
		results := make([]string, len(all))
		parallel(len(all), func(i int) { results[i] = probe(ns, pod, "connect", all[i].addr) })
		for i, t := range all {
			checkBlocked(t.label, results[i])
		}
		checkBlocked("external name", probe(ns, pod, "lookup", "example.com"))
		checkBlocked("DNS query straight to a public resolver", probe(ns, pod, "dns", "1.1.1.1:53", "example.com"))
		check("cluster DNS (positive control)", "resolved", probe(ns, pod, "lookup", "kubernetes.default.svc.cluster.local"))
	}
}

func hostControl() {
	for _, t := range []target{{"public IP", "1.1.1.1:443"}, {"second public IP", "8.8.8.8:53"}, {"HTTPS by name", "example.com:443"}} {
		check("online host to "+t.label+" (control)", "reached", fromHost(t.addr))
	}
}

func ipv6Checks(pod [2]string) {
	section("IPv6")
	out, err := vm("", "sysctl -n net.ipv6.conf.all.disable_ipv6; ip -6 -o addr show scope global | wc -l; ip -6 -o addr show dev eth0 scope link | wc -l")
	if err != nil {
		fatal("read IPv6 state: %v", err)
	}
	info("VM IPv6 state, disabled flag / global addresses / link-local on eth0: %s", strings.ReplaceAll(out, "\n", " / "))
	check("VM has no global IPv6 address", "0", strings.Fields(out)[1])
	checkBlocked("pod to public IPv6", probe(pod[0], pod[1], "connect", "[2606:4700:4700::1111]:443"))
	result, err := vm("", "timeout 6 bash -c '</dev/tcp/2606:4700:4700::1111/443' 2>&1; echo rc=$?")
	if err != nil {
		fatal("node IPv6: %v", err)
	}
	if strings.Contains(result, "rc=0") {
		check("VM to public IPv6", "blocked", "reached")
	} else {
		fmt.Println("ok   VM to public IPv6: blocked")
	}
	// The host's table is inet, so its drop covers IPv6. A link-local address is the one route the
	// isolated bridge could offer. Without one, this is not applicable, and says so.
	addr, err := exec.Command("sh", "-c", "ip -6 -o addr show dev virbr-airgap scope link | awk '{sub(\"/.*\",\"\",$4); print $4; exit}'").Output()
	if err != nil {
		fatal("host IPv6: %v", err)
	}
	link := strings.TrimSpace(string(addr))
	if link == "" {
		info("not applicable: the host bridge has no link-local IPv6 address")
		return
	}
	got, err := vm("", "timeout 6 bash -c '</dev/tcp/"+link+"%eth0/22' 2>&1; echo rc=$?")
	if err != nil {
		fatal("node to host link-local: %v", err)
	}
	if strings.Contains(got, "rc=0") {
		check("VM to host link-local SSH", "blocked", "reached")
	} else {
		fmt.Println("ok   VM to host link-local SSH: blocked")
	}
}

func matrix(pod [2]string, others [][2]string, system [2]string) {
	ns, name := pod[0], pod[1]
	tcpTargets := []target{{"canary", canaryIP + ":" + canaryTCP}, {"metadata address", metadataIP + ":" + canaryTCP}}

	cell := func(state string, reached bool) {
		section("layer matrix: " + state)
		for _, t := range tcpTargets {
			before := theCanary.tcpCount(t.addr)
			var got string
			if reached {
				got = probeUntil("reached", ns, name, "connect", t.addr)
				check(t.label+" from a pod", "reached", got)
			} else {
				time.Sleep(settle)
				got = probe(ns, name, "connect", t.addr)
				checkBlocked(t.label+" from a pod", got)
			}
			arrived := "not-logged"
			if theCanary.tcpCount(t.addr) > before {
				arrived = "logged"
			}
			want := "not-logged"
			if reached {
				want = "logged"
			}
			check(t.label+" listener", want, arrived)
		}
		label := uniqueLabel()
		query := probe(ns, name, "dns", canaryIP+":"+canaryDNS, label+".canary.test")
		if reached {
			check("direct DNS query answered", "answered", query)
			check("direct DNS query in the canary log", "logged", theCanary.logged(label))
		} else {
			checkBlocked("direct DNS query", query)
			check("direct DNS query in the canary log", "not-logged", theCanary.logged(label))
		}
	}

	cell("VM path closed, policy on", false)

	policyOff()
	cell("VM path closed, policy off", false)
	section("VM path closed, policy off: the same state shows which layer holds")
	check("API on node (control: policy is what blocked it)", "reached", probeUntil("reached", ns, name, "connect", vmIP+":6443"))
	check("kubelet (control)", "reached", probeUntil("reached", ns, name, "connect", vmIP+":10250"))
	check("API service (control)", "reached", probeUntil("reached", ns, name, "connect", "10.43.0.1:443"))
	info("gateway with the policy off: %s", probe(ns, name, "connect", "169.254.1.1:80"))
	bridge := hostIP + ":" + bridgeTCP
	checkBlocked("host listener from a pod, firewall in place", probe(ns, name, "connect", bridge))
	checkBlocked("host listener from the VM, firewall in place", fromNode(bridge))
	checkBlocked("kube-system pod to public IP", probe(system[0], system[1], "connect", "1.1.1.1:443"))
	checkBlocked("kube-system pod to the canary", probe(system[0], system[1], "connect", canaryIP+":"+canaryTCP))
	checkBlocked("VM to public IP", fromNode("1.1.1.1:443"))
	checkBlocked("VM to the canary", fromNode(canaryIP+":"+canaryTCP))

	mustHost("nft", "insert", "rule", "inet", "airgapai", "input", "iifname", "virbr-airgap", "ip", "daddr", hostIP, "tcp", "dport", bridgeTCP, "accept")
	check("host listener from a pod, firewall opened (control)", "reached", probeUntil("reached", ns, name, "connect", bridge))
	check("host listener from the VM, firewall opened (control)", "reached", fromNode(bridge))
	mustHost("nft", "-f", "/etc/airgapai/firewall.nft")

	openVMPath()
	cell("VM path open, policy off", true)

	policyOn()
	cell("VM path open, policy on", false)
	section("VM path open, policy on: other namespaces")
	check("kube-system pod to the canary (control: the path is open)", "reached", probeUntil("reached", system[0], system[1], "connect", canaryIP+":"+canaryTCP))
	check("VM to the canary (control: the path is open)", "reached", fromNode(canaryIP+":"+canaryTCP))
	results := make([]string, len(others))
	parallel(len(others), func(i int) { results[i] = probe(others[i][0], others[i][1], "connect", canaryIP+":"+canaryTCP) })
	for i, other := range others {
		checkBlocked(other[0]+" to the canary", results[i])
	}

	closeVMPath()
	section("back to closed")
	time.Sleep(settle)
	checkBlocked("canary from a pod", probe(ns, name, "connect", canaryIP+":"+canaryTCP))
	checkBlocked("kube-system pod to the canary", probe(system[0], system[1], "connect", canaryIP+":"+canaryTCP))
	checkBlocked("VM to the canary", fromNode(canaryIP+":"+canaryTCP))
}

// dnsCanary tests the path a pod always has, its cluster DNS. Only a forwarder could carry a name out.
func dnsCanary(pod [2]string) {
	section("DNS canary through cluster DNS")
	lookup := func() (string, string) {
		label := uniqueLabel()
		probe(pod[0], pod[1], "lookup", label+".canary.test.")
		time.Sleep(2 * time.Second)
		return label, theCanary.logged(label)
	}
	original := corefile()
	if strings.Contains(original, "forward ") {
		fatal("CoreDNS already has a forward plugin")
	}
	openVMPath()
	_, got := lookup()
	check("VM path open, no forwarder", "not-logged", got)

	forwarding := cacheLine.ReplaceAllString(original, "    forward . "+canaryIP+":"+canaryDNS+"\n$0")
	if !strings.Contains(forwarding, "forward ") {
		fatal("could not add a forwarder to the CoreDNS configuration")
	}
	setCorefile(forwarding)
	var logged string
	for range 5 { // a restarted CoreDNS can take a moment to serve
		if _, logged = lookup(); logged == "logged" {
			break
		}
		time.Sleep(3 * time.Second)
	}
	check("VM path open, forwarder added (control)", "logged", logged)

	closeVMPath()
	time.Sleep(settle)
	_, got = lookup()
	check("VM path closed, forwarder added", "not-logged", got)

	setCorefile(original)
	check("CoreDNS has no forward plugin again", "0", fmt.Sprint(strings.Count(corefile(), "forward ")))
}

func zotAccess(image string) {
	section("pods to Zot, through an allow rule that exists only for this test")
	ns := "egress-zot"
	addr := registry
	mustKube("create ns " + ns)
	if out, err := apply(podYAML(ns, "client", image, false), ""); err != nil {
		fatal("zot client: %v: %s", err, out)
	}
	mustKube("-n " + ns + " wait --for=condition=Ready pod/client --timeout=120s")
	checkBlocked("no allow rule", probe(ns, "client", "connect", addr))
	checkBlocked("other test namespace, no allow rule", probe("egress-new", "client", "connect", addr))
	rule := `apiVersion: crd.projectcalico.org/v1
kind: GlobalNetworkPolicy
metadata: {name: egress-test-zot-client}
spec:
  namespaceSelector: kubernetes.io/metadata.name == "egress-zot"
  types: [Egress]
  egress:
    - action: Allow
      protocol: TCP
      destination:
        namespaceSelector: kubernetes.io/metadata.name == "zot"
        ports: [5000]
---
apiVersion: crd.projectcalico.org/v1
kind: GlobalNetworkPolicy
metadata: {name: egress-test-zot-server}
spec:
  selector: app == "zot"
  namespaceSelector: kubernetes.io/metadata.name == "zot"
  types: [Ingress]
  ingress:
    - action: Allow
      protocol: TCP
      source:
        namespaceSelector: kubernetes.io/metadata.name == "egress-zot"
      destination:
        ports: [5000]
`
	if out, err := apply(rule, ""); err != nil {
		fatal("zot rule: %v: %s", err, out)
	}
	check("allow rule, named namespace", "reached", probeUntil("reached", ns, "client", "connect", addr))
	checkBlocked("allow rule, other namespace", probe("egress-new", "client", "connect", addr))
	checkBlocked("allow rule, same pod to the node API", probe(ns, "client", "connect", vmIP+":6443"))
	mustKube("delete globalnetworkpolicies.crd.projectcalico.org egress-test-zot-client egress-test-zot-server")
	time.Sleep(settle)
	checkBlocked("allow rule removed", probe(ns, "client", "connect", addr))
}

func admissionChecks(signed, unsigned string) {
	section("admission in a workload namespace")
	ns := "egress-new"
	check("signed compliant pod (control)", "admitted", admission(podYAML(ns, "dry", signed, false), "denied"))
	check("hostNetwork pod", "rejected", admission(podYAML(ns, "dry", signed, true), "violates PodSecurity"))
	check("unsigned image", "rejected", admission(podYAML(ns, "dry", unsigned, false), "denied the request: Policy require-signed-images failed"))
}

func registryOutage(signed, unsigned string) {
	section("registry outage")
	ns := "egress-outage"
	mustKube("create ns " + ns)
	if out, err := apply(podYAML(ns, "running", signed, false), ""); err != nil {
		fatal("outage pod: %v: %s", err, out)
	}
	mustKube("-n " + ns + " wait --for=condition=Ready pod/running --timeout=120s")
	id := strings.TrimPrefix(mustKube("-n "+ns+" get pod running -o jsonpath={.status.containerStatuses[0].containerID}"), "containerd://")

	mustKube("-n zot scale deployment zot --replicas=0")
	mustKube("-n zot wait --for=delete pod -l app=zot --timeout=120s")
	// Refused, not dropped: the service has no endpoints, so kube-proxy rejects the connection.
	check("registry from the VM (control: it is down)", "refused", fromNode(registry))
	check("running pod continues", "true", mustKube("-n "+ns+" get pod running -o jsonpath={.status.containerStatuses[0].ready}"))

	if _, err := vm("", crictl+"stop "+id); err != nil {
		fatal("stop container: %v", err)
	}
	restarted := "no"
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		out := mustKube("-n " + ns + " get pod running -o jsonpath={.status.containerStatuses[0].restartCount}/{.status.containerStatuses[0].ready}")
		if out == "1/true" {
			restarted = "yes"
			break
		}
	}
	check("container restarts from the cached image", "yes", restarted)

	marker := "denied the request: Policy require-signed-images"
	check("new unsigned pod", "rejected", admission(podYAML(ns, "new-unsigned", unsigned, false), marker))
	// Not a plan requirement, and worth knowing: the verifier needs the registry for signed images too.
	info("new signed pod during the outage: %s", admission(podYAML(ns, "new-signed", signed, false), marker))

	mustKube("-n zot scale deployment zot --replicas=1")
	mustKube("-n zot rollout status deployment/zot --timeout=180s")
	check("registry back (control)", "reached", fromNode(registry))
	var back string
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		if back = admission(podYAML(ns, "after", signed, false), marker); back == "admitted" {
			break
		}
	}
	check("new signed pod after the outage", "admitted", back)
}

func startPod(ns, image string) {
	if out, err := apply(podYAML(ns, "egress-probe", image, false), ""); err != nil {
		fatal("pod in %s: %v: %s", ns, err, out)
	}
}

func main() {
	flag.StringVar(&root, "root", "", "repository root")
	flag.Parse()
	if root == "" {
		fmt.Println("usage: egress -root <repository>")
		os.Exit(2)
	}
	lock, err := os.OpenFile("/mnt/data/airgapai/.lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fmt.Println("FAIL lock:", err)
		os.Exit(1)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("FAIL another bundle or VM command is running")
		os.Exit(1)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		fmt.Println("interrupted, closing everything")
		cleanup()
		os.Exit(1)
	}()

	section("preflight")
	cmd := exec.Command("sudo", "-n", "nft", "list", "table", "inet", "airgapai")
	table, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(table), `iifname "virbr-airgap" drop`) {
		fmt.Println("FAIL the host firewall table is missing, so no result here would mean anything")
		os.Exit(1)
	}
	fmt.Println("ok   host firewall table present")
	installed, err1 := os.ReadFile("/etc/airgapai/firewall.nft")
	tracked, err2 := os.ReadFile(root + "/infra/firewall.nft")
	if err1 != nil || err2 != nil || string(installed) != string(tracked) {
		fmt.Println("FAIL the installed host firewall differs from the repository")
		os.Exit(1)
	}
	fmt.Println("ok   installed firewall matches the repository")

	cleanup() // whatever an interrupted run left
	failures = 0
	for _, ns := range testNS {
		vm("", kubectl+"wait --for=delete ns/"+ns+" --timeout=120s 2>/dev/null")
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("FAIL", r)
			cleanup()
			os.Exit(1)
		}
	}()

	mustHost("ip", "link", "add", canaryLink, "type", "dummy")
	mustHost("ip", "addr", "add", canaryIP+"/32", "dev", canaryLink)
	mustHost("ip", "addr", "add", metadataIP+"/32", "dev", canaryLink)
	mustHost("ip", "link", "set", canaryLink, "up")
	theCanary = startCanary()

	signed := imageDigest("probe")
	unsigned := imageDigest("probe-unsigned")

	check("default deny installed", "default-deny-workloads", mustKube("get "+policyName+" -o jsonpath={.metadata.name}"))
	check("CoreDNS has no forward plugin", "0", fmt.Sprint(strings.Count(corefile(), "forward ")))

	mustKube("create ns egress-new")
	pods := [][2]string{{"egress-new", "client"}, {"workloads", "egress-probe"}}
	others := [][2]string{{"forgejo", "egress-probe"}, {"argocd", "egress-probe"}, {"kyverno", "egress-probe"}, {"zot", "egress-probe"}}
	system := [2]string{"kube-system", "egress-probe"}
	if out, err := apply(podYAML("egress-new", "client", signed, false), ""); err != nil {
		fatal("client: %v: %s", err, out)
	}
	for _, p := range append(slices.Clone(others), [2]string{"workloads", "egress-probe"}, system) {
		startPod(p[0], signed)
	}
	for _, p := range append(slices.Clone(others), pods[0], pods[1], system) {
		mustKube(fmt.Sprintf("-n %s wait --for=condition=Ready pod/%s --timeout=180s", p[0], p[1]))
	}

	section("online host, before the pods try the same targets")
	hostControl()
	publicChecks(pods, hostListeners())
	section("online host, after the pods tried the same targets")
	hostControl()

	section("VM node and system namespace with the path closed")
	checkBlocked("VM to public IP", fromNode("1.1.1.1:443"))
	checkBlocked("VM to host SSH", fromNode(hostIP+":22"))
	checkBlocked("kube-system pod to public IP", probe(system[0], system[1], "connect", "1.1.1.1:443"))

	ipv6Checks(pods[0])
	matrix(pods[0], others, system)
	dnsCanary(pods[0])
	admissionChecks(signed, unsigned)
	zotAccess(signed)
	registryOutage(signed, unsigned)

	section("closing")
	cleanup()
	table, err = exec.Command("sudo", "-n", "nft", "list", "table", "inet", "airgapai").CombinedOutput()
	if err != nil || strings.Contains(string(table), canaryIP) || strings.Contains(string(table), bridgeTCP) {
		fmt.Println("FAIL the host firewall still holds a test rule")
		failures++
	} else {
		fmt.Println("ok   host firewall back to its installed rules")
	}
	if routes, err := vm("", "ip route"); err != nil || strings.Contains(routes, canaryIP) || strings.Contains(routes, metadataIP) {
		fmt.Println("FAIL the VM still holds a test route")
		failures++
	} else {
		fmt.Println("ok   VM routes back to normal")
	}
	check("default deny restored", "default-deny-workloads", mustKube("get "+policyName+" -o jsonpath={.metadata.name}"))

	fmt.Println()
	if failures != 0 {
		fmt.Printf("%d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("all egress checks passed")
}
