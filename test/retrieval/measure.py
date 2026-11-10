#!/usr/bin/env python3
"""Measure the retrieval service in the VM: recall per role against an exact scan, and latency.

For each query in queries.txt and each reader role, the service is asked three times: with the HNSW
index and iterative scans (the default), with the index and no iterative scans, and with an exact
scan of every row the role may see. Recall at k is the share of the exact top k that a scan also
returned. Requests go through kubectl port-forward over SSH, which does not pass through the
network policy, and their latency includes embedding the query.
"""
import argparse
import datetime
import json
import pathlib
import statistics
import subprocess
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[2]
PORT = 18190
URL = f"http://127.0.0.1:{PORT}"
KUBECTL = ("sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml"
           " -n retrieval")
VM_SSH = str(ROOT / "infra/vm-ssh.sh")
ROLES = ["technician", "engineer"]
SCANS = ["hnsw", "hnsw-strict", "exact"]
KS = [5, 10, 20]


def vm(command):
    return subprocess.run([VM_SSH, command], capture_output=True, text=True, check=True).stdout


def tunnel():
    """Forward the service to the host. The terminal makes the remote forward end with SSH."""
    process = subprocess.Popen(
        [VM_SSH, "-tt", "-L", f"{PORT}:127.0.0.1:{PORT}",
         f"{KUBECTL} port-forward svc/retrieval {PORT}:8080 >/dev/null"],
        stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(60):
        try:
            urllib.request.urlopen(f"{URL}/ready", timeout=2).close()
            return process
        except (urllib.error.URLError, ConnectionError, TimeoutError):
            time.sleep(1)
    process.terminate()
    raise SystemExit("port-forward did not come up")


def search(query, role, k, scan):
    body = json.dumps({"query": query, "role": role, "k": k, "scan": scan}).encode()
    request = urllib.request.Request(f"{URL}/search", body, {"Content-Type": "application/json"})
    start = time.monotonic()
    with urllib.request.urlopen(request, timeout=60) as response:
        results = json.load(response)["results"]
    return results, time.monotonic() - start


def key(results):
    return [(r["path"], r["chunk_index"]) for r in results]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=pathlib.Path, help="directory for the result JSON")
    args = parser.parse_args()
    queries = [q for q in (ROOT / "test/retrieval/queries.txt").read_text().splitlines()
               if q.strip() and not q.startswith("#")]
    image = json.loads(vm(f"{KUBECTL} get deployment retrieval -o json"))[
        "spec"]["template"]["spec"]["containers"][0]["image"]
    # Every connection needs a password; the superuser's is in the pod's environment.
    sql = "SELECT classification, count(*) FROM rag.chunks GROUP BY 1 ORDER BY 1"
    rows = vm(f"{KUBECTL} exec deploy/postgres -- sh -c 'PGPASSWORD=\"$POSTGRES_PASSWORD\""
              f" psql -h 127.0.0.1 -U postgres -d rag -AtF= -c \"{sql}\"'")

    process = tunnel()
    try:
        search(queries[0], "engineer", 5, "hnsw")  # warm up
        per_query = []
        for query in queries:
            entry = {"query": query}
            top = {}  # each role's default results at k=5, kept from the loop below
            for role in ROLES:
                for k in KS:
                    found = {scan: search(query, role, k, scan) for scan in SCANS}
                    exact = set(key(found["exact"][0]))
                    # With no rows to find, recall is undefined rather than zero or one.
                    entry[f"{role}@{k}"] = {
                        scan: {"returned": len(results),
                               "recall": len(exact & set(key(results))) / len(exact)
                               if exact else None,
                               "latency_ms": round(seconds * 1000, 1)}
                        for scan, (results, seconds) in found.items()}
                    if k == 5:
                        top[role] = found["hnsw"][0]
            entry["roles_differ_at_5"] = key(top["technician"]) != key(top["engineer"])
            entry["engineer_top5_restricted"] = sum(
                r["classification"] == "restricted" for r in top["engineer"])
            per_query.append(entry)
    finally:
        process.terminate()

    summary = {}
    for role in ROLES:
        for k in KS:
            for scan in SCANS:
                cells = [q[f"{role}@{k}"][scan] for q in per_query]
                recalls = [c["recall"] for c in cells if c["recall"] is not None]
                summary[f"{role} k={k} {scan}"] = {
                    "mean_recall": round(statistics.mean(recalls), 3) if recalls else None,
                    "min_recall": round(min(recalls), 3) if recalls else None,
                    "queries_with_nothing_to_find": len(cells) - len(recalls),
                    "queries_short_of_k": sum(c["returned"] < k for c in cells),
                    "median_latency_ms": round(
                        statistics.median(c["latency_ms"] for c in cells), 1),
                }
    report = {
        "when": datetime.datetime.now(datetime.UTC).isoformat(timespec="seconds"),
        "where": "VM pods, requests over kubectl port-forward, latency includes query embedding",
        "image": image,
        "rows": rows.split(),
        "queries": len(queries),
        "roles_differ_at_5": sum(q["roles_differ_at_5"] for q in per_query),
        "summary": summary,
        "per_query": per_query,
    }
    width = max(map(len, summary))
    for name, s in summary.items():
        recall = (f"recall {s['mean_recall']:.3f} (min {s['min_recall']:.3f})"
                  if s["mean_recall"] is not None else "recall undefined, nothing to find")
        print(f"{name:<{width}}  {recall}  short of k: {s['queries_short_of_k']:>2}"
              f"  median {s['median_latency_ms']} ms")
    differ = report["roles_differ_at_5"]
    print(f"queries where the two roles' top 5 differ: {differ} of {len(queries)}")
    if args.out:
        args.out.mkdir(parents=True, exist_ok=True)
        path = args.out / "retrieval.json"
        path.write_text(json.dumps(report, indent=2) + "\n")
        print(f"saved {path}")


if __name__ == "__main__":
    main()
