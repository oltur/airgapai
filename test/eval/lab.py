"""Reaching the lab's VM from the host, for the evals: commands over SSH, and the retrieval service
through kubectl port-forward, which does not pass through the network policy."""
import contextlib
import json
import pathlib
import subprocess
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[2]
VM_SSH = str(ROOT / "infra/vm-ssh.sh")
KUBECTL = "sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml"
PORT = 18191
URL = f"http://127.0.0.1:{PORT}"


def vm(command):
    return subprocess.run([VM_SSH, command], capture_output=True, text=True, check=True).stdout


@contextlib.contextmanager
def tunnel():
    """Forward the retrieval service to the host. The terminal makes the remote forward end with
    SSH."""
    process = subprocess.Popen(
        [VM_SSH, "-tt", "-L", f"{PORT}:127.0.0.1:{PORT}",
         f"{KUBECTL} -n retrieval port-forward svc/retrieval {PORT}:8080 >/dev/null"],
        stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(60):
            try:
                urllib.request.urlopen(f"{URL}/ready", timeout=2).close()
                break
            except (urllib.error.URLError, ConnectionError, TimeoutError):
                time.sleep(1)
        else:
            raise SystemExit("port-forward did not come up")
        yield
    finally:
        process.terminate()
        process.wait(timeout=10)


def search(query, role, k):
    body = json.dumps({"query": query, "role": role, "k": k}).encode()
    request = urllib.request.Request(f"{URL}/search", body, {"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.load(response)["results"]


def deployed():
    """What the run measured: the images that make retrieval what it is, and the rows by label."""
    images = {}
    for namespace, kind, name in [("retrieval", "deployment", "retrieval"),
                                  ("retrieval", "job", "ingest"),
                                  ("inference", "deployment", "embed")]:
        found = json.loads(vm(f"{KUBECTL} -n {namespace} get {kind} {name} -o json"))
        spec = found["spec"]["template"]["spec"]
        images[name] = [c["image"] for c in spec.get("initContainers", []) + spec["containers"]]
    # Every connection needs a password; the superuser's is in the pod's environment.
    sql = "SELECT classification, count(*) FROM rag.chunks GROUP BY 1 ORDER BY 1"
    rows = vm(f"{KUBECTL} -n retrieval exec deploy/postgres -- sh -c"
              " 'PGPASSWORD=\"$POSTGRES_PASSWORD\""
              f" psql -h 127.0.0.1 -U postgres -d rag -AtF= -c \"{sql}\"'")
    return {"images": images, "rows": rows.split()}
