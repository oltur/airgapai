#!/usr/bin/env python3
"""Measure one GGUF model under llama-server: peak memory, load time and token speed.

The server runs in a systemd scope with a memory cap and no swap, bound to localhost, with
4 threads to match the VM's 4 vCPUs. This is a host measurement, not one from inside the VM.
"""
import argparse
import json
import pathlib
import statistics
import subprocess
import threading
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path("/mnt/data/airgapai/spike")
BIN = ROOT / "llama.cpp/bin/llama-b12032"
PORT = 18080
URL = f"http://127.0.0.1:{PORT}"
CAP_MIB = 5000
HOST_MARGIN_MIB = 3072  # The same margin as infra/versions.env.
THREADS = 4
CONTEXT = 4096
RUNS = 3
PREDICT = 128
PROMPT = "List the steps to safely replace a worn bearing on an industrial conveyor."


def available_mib():
    for line in open("/proc/meminfo"):
        if line.startswith("MemAvailable:"):
            return int(line.split()[1]) // 1024
    raise RuntimeError("MemAvailable not found")


def cgroup_dir(pid):
    relative = open(f"/proc/{pid}/cgroup").read().strip().split("::", 1)[1]
    return pathlib.Path("/sys/fs/cgroup" + relative)


def vm_hwm_mib(pid):
    for line in open(f"/proc/{pid}/status"):
        if line.startswith("VmHWM:"):
            return int(line.split()[1]) // 1024
    return None


def watchdog(server, stop, tripped):
    """Kill the server if the host runs short, so the spike cannot hurt other workloads."""
    while not stop.wait(1):
        if available_mib() < HOST_MARGIN_MIB:
            tripped.set()
            server.kill()
            return


def wait_healthy(server, timeout=180):
    start = time.monotonic()
    while time.monotonic() - start < timeout:
        if server.poll() is not None:
            raise RuntimeError(f"server exited with {server.returncode} while loading")
        try:
            with urllib.request.urlopen(f"{URL}/health", timeout=2) as response:
                if response.status == 200:
                    return time.monotonic() - start
        except (urllib.error.URLError, TimeoutError, ConnectionError):
            pass
        time.sleep(0.2)
    raise RuntimeError("server not healthy in time")


def generate():
    body = json.dumps({
        "messages": [{"role": "user", "content": PROMPT}],
        "max_tokens": PREDICT,
        "temperature": 0,
        "seed": 0,
        "cache_prompt": False,
        "ignore_eos": True,  # Every run and model produces the same number of tokens.
    }).encode()
    request = urllib.request.Request(
        f"{URL}/v1/chat/completions", body, {"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=600) as response:
        return json.load(response)["timings"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("name", help="directory under the spike root, such as granite")
    parser.add_argument("--out", type=pathlib.Path, help="directory for the result JSON")
    args = parser.parse_args()

    gguf = next((ROOT / args.name).glob("*.gguf"))
    before = available_mib()
    if before < CAP_MIB + HOST_MARGIN_MIB:
        raise SystemExit(f"only {before} MiB available, need {CAP_MIB + HOST_MARGIN_MIB}")

    server = subprocess.Popen(
        ["systemd-run", "--user", "--scope", "--quiet",
         "-p", f"MemoryMax={CAP_MIB}M", "-p", "MemorySwapMax=0",
         "env", f"LD_LIBRARY_PATH={BIN}", str(BIN / "llama-server"),
         "-m", str(gguf), "--host", "127.0.0.1", "--port", str(PORT),
         "-t", str(THREADS), "-c", str(CONTEXT), "--parallel", "1"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, cwd="/")

    stop, tripped = threading.Event(), threading.Event()
    threading.Thread(target=watchdog, args=(server, stop, tripped), daemon=True).start()
    try:
        load_s = wait_healthy(server)
        timings = [generate() for _ in range(RUNS)]
        group = cgroup_dir(server.pid)
        result = {
            "model": gguf.name,
            "threads": THREADS,
            "context": CONTEXT,
            "cap_mib": CAP_MIB,
            "host_available_before_mib": before,
            "load_s": round(load_s, 1),
            "cgroup_peak_mib": int((group / "memory.peak").read_text()) // 2**20,
            "process_peak_rss_mib": vm_hwm_mib(server.pid),
            "cgroup_oom_kills": int(dict(
                line.split() for line in (group / "memory.events").read_text().splitlines()
            )["oom_kill"]),
            "generate_tokens_per_s": [round(t["predicted_per_second"], 1) for t in timings],
            "generate_median": round(
                statistics.median(t["predicted_per_second"] for t in timings), 1),
            "prompt_tokens_per_s": [round(t["prompt_per_second"], 1) for t in timings],
            "tokens_predicted": [t["predicted_n"] for t in timings],
        }
    finally:
        stop.set()
        server.terminate()
        try:
            server.wait(timeout=15)
        except subprocess.TimeoutExpired:
            server.kill()
        if tripped.is_set():
            print("stopped: the host fell below its memory margin")

    print(json.dumps(result, indent=2))
    if args.out:
        args.out.mkdir(parents=True, exist_ok=True)
        (args.out / f"{args.name}.json").write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    main()
