#!/usr/bin/env python3
"""Measure one GGUF model under llama-server: peak memory, load time and token speed.

The server runs in a systemd scope with a memory cap and no swap, bound to localhost, with
4 threads to match the VM's 4 vCPUs. This is a host measurement, not one from inside the VM.
"""
import argparse
import contextlib
import datetime
import json
import pathlib
import socket
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
CHAT_MODELS = ["granite", "smollm3", "ministral"]
THREADS = 4
CONTEXT = 4096
RUNS = 3
PREDICT = 128
PROMPT = "List the steps to safely replace a worn bearing on an industrial conveyor."


def find_gguf(name):
    found = sorted((ROOT / name).glob("*.gguf"))
    if not found:
        raise SystemExit(f"no model in {ROOT / name}, run fetch.sh first")
    return found[0]


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
        time.sleep(0.05)
    raise RuntimeError("server not healthy in time")


def memory_figures(pid):
    """Read the peaks while the server runs. Process RSS is the figure to compare between runs:
    the cgroup peak omits model file pages that an earlier run already pulled into the cache."""
    group = cgroup_dir(pid)
    return {
        "cap_mib": int((group / "memory.max").read_text()) // 2**20,
        "cgroup_peak_mib": int((group / "memory.peak").read_text()) // 2**20,
        "process_peak_rss_mib": vm_hwm_mib(pid),
        "cgroup_oom_kills": int(dict(
            line.split() for line in (group / "memory.events").read_text().splitlines()
        )["oom_kill"]),
    }


@contextlib.contextmanager
def capped_server(gguf, *flags, cap_mib=CAP_MIB):
    """Run llama-server in a capped scope. Yield its process and how long it took to answer.

    Raises if the port is already taken, because a leftover server would answer health checks and
    be measured in this model's name. Raises on leaving if the watchdog fired or the server died,
    so such a run is never saved as a result. The load time has a 0.05 s polling resolution, and
    the model file may already be in the page cache, so it is not a cold-start figure.
    """
    if available_mib() < cap_mib + HOST_MARGIN_MIB:
        raise SystemExit(f"under {cap_mib + HOST_MARGIN_MIB} MiB available")
    with socket.socket() as probe:
        if probe.connect_ex(("127.0.0.1", PORT)) == 0:
            raise SystemExit(f"port {PORT} is in use, is a server still running?")

    server = subprocess.Popen(
        ["systemd-run", "--user", "--scope", "--quiet",
         "-p", f"MemoryMax={cap_mib}M", "-p", "MemorySwapMax=0",
         "env", f"LD_LIBRARY_PATH={BIN}", str(BIN / "llama-server"),
         "-m", str(gguf), "--host", "127.0.0.1", "--port", str(PORT),
         "-t", str(THREADS), "--parallel", "1", *flags],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, cwd="/")
    stop, tripped = threading.Event(), threading.Event()
    threading.Thread(target=watchdog, args=(server, stop, tripped), daemon=True).start()
    try:
        yield server, wait_healthy(server)
    finally:
        stop.set()
        exit_code = server.poll()
        server.terminate()
        try:
            server.wait(timeout=15)
        except subprocess.TimeoutExpired:
            server.kill()
            server.wait()
        if tripped.is_set():
            raise RuntimeError("stopped: the host fell below its memory margin")
        if exit_code is not None:
            raise RuntimeError(f"the server died during the run, exit code {exit_code}")


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
    parser.add_argument("name", choices=CHAT_MODELS, help="directory under the spike root")
    parser.add_argument("--out", type=pathlib.Path, help="directory for the result JSON")
    args = parser.parse_args()

    gguf = find_gguf(args.name)
    before = available_mib()
    with capped_server(gguf, "-c", str(CONTEXT)) as (server, load_s):
        timings = [generate() for _ in range(RUNS)]
        result = {
            "measured": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
            "llama_cpp": BIN.name,
            "model": gguf.name,
            "threads": THREADS,
            "context": CONTEXT,
            "host_available_before_mib": before,
            "load_s": round(load_s, 1),
            **memory_figures(server.pid),
            "generate_tokens_per_s": [round(t["predicted_per_second"], 1) for t in timings],
            "generate_median": round(
                statistics.median(t["predicted_per_second"] for t in timings), 1),
            "prompt_tokens_per_s": [round(t["prompt_per_second"], 1) for t in timings],
            "tokens_predicted": [t["predicted_n"] for t in timings],
        }

    print(json.dumps(result, indent=2))
    if args.out:
        args.out.mkdir(parents=True, exist_ok=True)
        (args.out / f"{args.name}.json").write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    main()
