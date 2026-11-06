#!/usr/bin/env python3
"""Measure the model servers deployed in the VM: start-up, memory, speed and the tool screen.

Runs on the lab host. Both pods are restarted first, so start-up includes copying each model out of
its image, though the images and some file pages may already be cached in the guest. Requests reach
the servers through kubectl port-forward over SSH, which does not pass through the network policy,
so this says nothing about who else can reach them. Requests, prompts and tool cases are the spike's
own (spike/model), so the results compare with ADR 0010's host figures.

Usage: test/inference/measure.py [--out DIR]
"""
import argparse
import datetime
import json
import pathlib
import statistics
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "spike/model"))
import embed  # noqa: E402
import measure  # noqa: E402
import toolcall  # noqa: E402

CHAT_PORT, EMBED_PORT = 18181, 18182
KUBECTL = ("sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml"
           " -n inference")
VM_SSH = str(ROOT / "infra/vm-ssh.sh")
VERSIONS = (ROOT / "infra/versions.env").read_text().splitlines()
VM_NAME = next(line.split("=", 1)[1] for line in VERSIONS if line.startswith("VM_NAME="))
APPS = ["chat", "embed"]


def vm(command):
    return subprocess.run([VM_SSH, command], capture_output=True, text=True, check=True).stdout


def pod(app):
    return json.loads(vm(f"{KUBECTL} get pod -l app={app} -o json"))["items"]


def restart(timeout=900):
    """Delete both pods, wait until their replacements are ready, and return start-up timings."""
    old = {item["metadata"]["name"] for app in APPS for item in pod(app)}
    vm(f"{KUBECTL} delete pod -l 'app in (chat,embed)' --wait=true")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        items = {app: [i for i in pod(app) if i["metadata"]["name"] not in old] for app in APPS}
        if all(len(v) == 1 and any(c["type"] == "Ready" and c["status"] == "True"
                                   for c in v[0]["status"].get("conditions", []))
               for v in items.values()):
            break
        time.sleep(5)
    else:
        raise SystemExit("pods not ready in time")
    timings = {}
    for app, (item,) in items.items():
        stamp = lambda text: datetime.datetime.fromisoformat(text.replace("Z", "+00:00"))  # noqa: E731
        status = item["status"]
        created = stamp(item["metadata"]["creationTimestamp"])
        copied = status["initContainerStatuses"][0]["state"]["terminated"]
        ready = next(c for c in status["conditions"] if c["type"] == "Ready")
        timings[app] = {
            "copy_s": (stamp(copied["finishedAt"]) - stamp(copied["startedAt"])).total_seconds(),
            "copy_log": vm(f"{KUBECTL} logs {item['metadata']['name']} -c copy-model").strip(),
            "created_to_ready_s": (stamp(ready["lastTransitionTime"]) - created).total_seconds(),
            "restarts": status["containerStatuses"][0]["restartCount"],
        }
    return timings


def tunnel():
    """Forward both services to the host. The terminal makes the remote forwards end with SSH."""
    forwards = " & ".join(f"{KUBECTL} port-forward svc/{app} {port}:8080 >/dev/null"
                          for app, port in zip(APPS, (CHAT_PORT, EMBED_PORT), strict=True))
    process = subprocess.Popen(
        [VM_SSH, "-tt", "-L", f"{CHAT_PORT}:127.0.0.1:{CHAT_PORT}",
         "-L", f"{EMBED_PORT}:127.0.0.1:{EMBED_PORT}", f"{forwards} & wait"],
        stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(60):
        try:
            for port in (CHAT_PORT, EMBED_PORT):
                urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=2).close()
            return process
        except (urllib.error.URLError, ConnectionError, TimeoutError):
            time.sleep(1)
    process.terminate()
    raise SystemExit("port-forward did not come up")


def memory(app):
    """The server is PID 1 in its container: its peak RSS, and its anonymous and file parts now."""
    status = dict(line.split(":", 1) for line in
                  vm(f"{KUBECTL} exec deploy/{app} -c server -- cat /proc/1/status").splitlines())
    mib = lambda key: int(status[key].split()[0]) // 1024  # noqa: E731
    cgroup = vm(f"{KUBECTL} exec deploy/{app} -c server -- cat /sys/fs/cgroup/memory.current "
                "/sys/fs/cgroup/memory.events").split()
    return {"peak_rss_mib": mib("VmHWM"), "rss_mib": mib("VmRSS"), "rss_anon_mib": mib("RssAnon"),
            "rss_file_mib": mib("RssFile"), "cgroup_current_mib": int(cgroup[0]) // 2**20,
            "cgroup_oom_kills": int(dict(zip(cgroup[1::2], cgroup[2::2], strict=True))["oom_kill"])}


def vm_process():
    """The VM's QEMU process: the one whose -name argument is exactly this guest."""
    for path in pathlib.Path("/proc").glob("[0-9]*"):
        try:
            args = (path / "cmdline").read_bytes().split(b"\0")
            if any(a.split(b",")[0] == f"guest={VM_NAME}".encode() for a in args):
                lines = (path / "status").read_text().splitlines()
                return dict(line.split(":", 1) for line in lines)
        except OSError:  # The process ended during the scan.
            continue
    raise SystemExit(f"no QEMU process for {VM_NAME}")


def pressure():
    """Host memory pressure: the share of time some task stalled on memory, over 60 and 300 s."""
    some = (pathlib.Path("/proc/pressure/memory").read_text().splitlines()[0]).split()[1:]
    return {key: float(value) for key, value in (field.split("=") for field in some)
            if key in ("avg60", "avg300")}


def host_memory():
    status = vm_process()
    guest = vm("free -m").split("\n")[1].split()
    return {"host_available_mib": measure.available_mib(),
            "host_memory_pressure_some_pct": pressure(),
            "vm_resident_mib": int(status["VmRSS"].split()[0]) // 1024,
            "vm_swap_mib": int(status["VmSwap"].split()[0]) // 1024,
            "guest_available_mib": int(guest[6])}


def chunk_texts(count, offset=0):
    return [f"search_document: {offset + i} {embed.CHUNK}" for i in range(count)]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=pathlib.Path, help="directory for the result JSON")
    args = parser.parse_args()

    startup = restart()
    forward = tunnel()
    try:
        measure.URL = toolcall.URL = f"http://127.0.0.1:{CHAT_PORT}"
        embed.URL = f"http://127.0.0.1:{EMBED_PORT}"
        with urllib.request.urlopen(f"{measure.URL}/props", timeout=10) as response:
            build = json.load(response).get("build_info")

        generated = [measure.generate() for _ in range(measure.RUNS)]
        query, _ = embed.embed(f"search_query: {embed.QUERY}")
        related, _ = embed.embed(f"search_document: {embed.RELATED}")
        unrelated, _ = embed.embed(f"search_document: {embed.UNRELATED}")
        chunks = [embed.timed_run(chunk_texts(embed.CHUNKS_PER_RUN)) for _ in range(embed.RUNS)]
        query_texts = [f"search_query: {i} {embed.QUERY}" for i in range(embed.QUERIES_PER_RUN)]
        queries = [embed.timed_run(query_texts) for _ in range(embed.RUNS)]
        latencies = [s * 1000 for run in queries for s in run[3]]

        cases = []
        for name, messages, tool, priority, must_contain, only_forbid in toolcall.CASES:
            choice = toolcall.chat(messages)
            problems = toolcall.judge(choice, tool, priority, must_contain, only_forbid)
            cases.append({"case": name, "passed": not problems, "problems": problems,
                          "reply": choice["message"]})

        # Both at once: one generation while the embedder works through chunks.
        together = {}

        def embed_meanwhile():
            try:
                together["chunks"] = embed.timed_run(chunk_texts(embed.CHUNKS_PER_RUN, offset=1000))
            except Exception as error:  # noqa: BLE001  Raised again below, in the main thread.
                together["error"] = error

        worker = threading.Thread(target=embed_meanwhile)
        worker.start()
        together["generate"] = measure.generate()
        worker.join()
        if "error" in together:
            raise together["error"]

        result = {
            "measured": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
            "where": "VM pods, requests over kubectl port-forward",
            "llama_cpp": build,
            "startup": startup,
            "chat": {
                "generate_tokens_per_s": [round(t["predicted_per_second"], 1) for t in generated],
                "generate_median": round(statistics.median(
                    t["predicted_per_second"] for t in generated), 1),
                "prompt_tokens_per_s": [round(t["prompt_per_second"], 1) for t in generated],
                "tokens_predicted": [t["predicted_n"] for t in generated],
                "tool_cases_passed": sum(c["passed"] for c in cases),
                "tool_cases": cases,
            },
            "embed": {
                "dimensions": len(query),
                "chunks_per_s": [round(c[0], 2) for c in chunks],
                "chunks_per_s_median": round(statistics.median(c[0] for c in chunks), 2),
                "tokens_per_chunk": chunks[0][2],
                "query_ms_median": round(statistics.median(latencies), 1),
                "query_ms_slowest": round(max(latencies), 1),
                "cosine_related": round(embed.cosine(query, related), 3),
                "cosine_unrelated": round(embed.cosine(query, unrelated), 3),
            },
            "together": {
                "generate_tokens_per_s": round(together["generate"]["predicted_per_second"], 1),
                "chunks_per_s": round(together["chunks"][0], 2),
            },
            "memory": {app: memory(app) for app in APPS},
            **host_memory(),
        }
    finally:
        forward.terminate()
        forward.wait()

    print(json.dumps({k: v for k, v in result.items() if k != "chat"}, indent=2))
    print(f"tool cases passed: {result['chat']['tool_cases_passed']} of {len(toolcall.CASES)}")
    if args.out:
        args.out.mkdir(parents=True, exist_ok=True)
        (args.out / "inference-vm.json").write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    main()
