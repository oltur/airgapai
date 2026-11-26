"""Reaching the lab's VM from the host, for the evals and measurements: commands over SSH, and the
retrieval service through kubectl port-forward, which does not pass through the network policy."""
import contextlib
import json
import pathlib
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[2]
VM_SSH = str(ROOT / "infra/vm-ssh.sh")
KUBECTL = "sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml"
PORT = 18191


def vm(command):
    return subprocess.run([VM_SSH, command], capture_output=True, text=True, check=True).stdout


def url(port=PORT):
    return f"http://127.0.0.1:{port}"


@contextlib.contextmanager
def tunnel(port=PORT, target="svc/retrieval"):
    """Forward the retrieval service, or another target in its namespace, to the host. The terminal
    makes the remote forward end with SSH. The port must be free, so that a forward left from
    another run is never measured."""
    with socket.socket() as probe:
        if probe.connect_ex(("127.0.0.1", port)) == 0:
            raise SystemExit(f"port {port} is already in use, perhaps by a forward left running")
    with tempfile.TemporaryFile("w+") as errors:
        process = subprocess.Popen(
            [VM_SSH, "-tt", "-L", f"{port}:127.0.0.1:{port}",
             f"{KUBECTL} -n retrieval port-forward {target} {port}:8080 >/dev/null"],
            stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=errors)
        try:
            for _ in range(60):
                if process.poll() is not None:
                    errors.seek(0)
                    raise SystemExit(f"the port-forward ended: {errors.read().strip()}")
                try:
                    urllib.request.urlopen(f"{url(port)}/ready", timeout=2).close()
                    break
                except (urllib.error.URLError, ConnectionError, TimeoutError):
                    time.sleep(1)
            else:
                raise SystemExit("port-forward did not come up")
            yield
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


def search(query, role, k, scan=None, port=PORT):
    """The retrieval service's results for one query, with the service's default scan unless one
    is named."""
    body = {"query": query, "role": role, "k": k}
    if scan:
        body["scan"] = scan
    request = urllib.request.Request(f"{url(port)}/search", json.dumps(body).encode(),
                                     {"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.load(response)["results"]


def chunk_counts():
    """The chunks in the table, by label, as the superuser, whose password is in the pod's
    environment."""
    sql = "SELECT classification, count(*) FROM rag.chunks GROUP BY 1 ORDER BY 1"
    return vm(f"{KUBECTL} -n retrieval exec deploy/postgres -- sh -c"
              " 'PGPASSWORD=\"$POSTGRES_PASSWORD\""
              f" psql -h 127.0.0.1 -U postgres -d rag -AtF= -c \"{sql}\"'").split()


RETRIEVAL = [("retrieval", "deployment", "retrieval"), ("retrieval", "job", "ingest"),
             ("inference", "deployment", "embed")]
GENERATION = RETRIEVAL + [("gateway", "deployment", "gateway"), ("inference", "deployment", "chat")]


def deployed(workloads=RETRIEVAL, args=False):
    """What the run measured: the images of the workloads that make the result what it is, and the
    rows by label. With args, also each container's arguments and environment, which change a
    server as much: the environment as written, so a secret appears only as its Secret's name."""
    images = {}
    for namespace, kind, name in workloads:
        found = json.loads(vm(f"{KUBECTL} -n {namespace} get {kind} {name} -o json"))
        spec = found["spec"]["template"]["spec"]
        containers = spec.get("initContainers", []) + spec["containers"]
        images[name] = [{"image": c["image"], "args": c.get("args", []), "env": c.get("env", [])}
                        if args else c["image"] for c in containers]
    return {"images": images, "rows": chunk_counts()}


def incomparable(baseline_asked, asked):
    """Why a run's questions cannot be compared with a baseline's, as a line; empty if they can.
    Each maps question ids to what was asked. The first difference is enough."""
    if set(baseline_asked) != set(asked):
        return ["the golden set's questions differ from the baseline's: save a new one"]
    changed = sorted(i for i, b in baseline_asked.items() if b != asked[i])
    if changed:
        return [f"questions changed since the baseline, which must be saved again: "
                f"{', '.join(changed)}"]
    return []


def redeployed(baseline_deployed, deployed):
    """A line if the deployment differs from the baseline's, empty if not. It fails the gate, but
    the rest of the comparison still runs and is reported: what else changed matters too."""
    if deployed is not None and baseline_deployed != deployed:
        return ["the deployed images, settings or chunk counts differ from the baseline's: "
                "a deliberate change needs a new baseline, saved in review"]
    return []


def in_vm(scripts, env):
    """Run the scripts, joined, as root in the VM, with env passed to them, and yield each line of
    their output as it comes. Fails if they exit non-zero."""
    source = "".join((ROOT / s).read_text() for s in scripts)
    assignments = [f"{k}={v}" for k, v in env.items()]
    with subprocess.Popen([VM_SSH, "sudo", *assignments, "bash", "-s"], stdin=subprocess.PIPE,
                          stdout=subprocess.PIPE, text=True) as process:
        process.stdin.write(source)
        process.stdin.close()
        # If this side stops, the script in the VM stops at its next line of output, not before.
        yield from (line.rstrip("\n") for line in process.stdout)
    if process.returncode:
        raise VMError(f"{' '.join(scripts)} exited {process.returncode} in the VM")


class VMError(Exception):
    """A script in the VM failed; what it printed before has been yielded."""
