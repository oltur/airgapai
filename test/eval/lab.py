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
def tunnel(port=PORT):
    """Forward the retrieval service to the host. The terminal makes the remote forward end with
    SSH. The port must be free, so that a forward left from another run is never measured."""
    with socket.socket() as probe:
        if probe.connect_ex(("127.0.0.1", port)) == 0:
            raise SystemExit(f"port {port} is already in use, perhaps by a forward left running")
    with tempfile.TemporaryFile("w+") as errors:
        process = subprocess.Popen(
            [VM_SSH, "-tt", "-L", f"{port}:127.0.0.1:{port}",
             f"{KUBECTL} -n retrieval port-forward svc/retrieval {port}:8080 >/dev/null"],
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


def deployed():
    """What the run measured: the images that make retrieval what it is, and the rows by label."""
    images = {}
    for namespace, kind, name in [("retrieval", "deployment", "retrieval"),
                                  ("retrieval", "job", "ingest"),
                                  ("inference", "deployment", "embed")]:
        found = json.loads(vm(f"{KUBECTL} -n {namespace} get {kind} {name} -o json"))
        spec = found["spec"]["template"]["spec"]
        images[name] = [c["image"] for c in spec.get("initContainers", []) + spec["containers"]]
    return {"images": images, "rows": chunk_counts()}
