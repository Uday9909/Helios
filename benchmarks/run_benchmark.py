#!/usr/bin/env python3
"""
Helios Benchmark Runner
Runs real load tests against a real server process, records real metrics to CSV,
generates real comparison plots. No synthetic data.
"""

import subprocess
import time
import csv
import os
import signal
import sys
import json
import urllib.request
import urllib.error
from pathlib import Path
import threading

# ── Config ────────────────────────────────────────────────────────────────────
SERVER_BINARY = "./helios-server"
BASE_URL = "http://localhost:8000"
PATTERNS = ["steady", "burst", "longtail"]
TEST_DURATION = "60s"
INPUT_DIM = int(os.environ.get("HELIOS_MODEL_INPUT_DIM", "128"))
OUTPUT_DIR = Path("benchmarks")
OUTPUT_DIR.mkdir(exist_ok=True)

CSV_FIELDS = [
    "timestamp", "rps", "p50", "p95", "p99",
    "cpu", "memory", "queue_depth", "active_workers",
    "max_workers", "batch_size"
]


def wait_for_server(timeout=30):
    """Poll /health until server is ready or timeout."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"{BASE_URL}/health", timeout=2) as r:
                if r.status == 200:
                    return True
        except Exception:
            pass
        time.sleep(0.5)
    return False


def fetch_metrics():
    """Fetch /metrics. Returns dict or None if server unavailable."""
    try:
        with urllib.request.urlopen(f"{BASE_URL}/metrics", timeout=3) as r:
            return json.loads(r.read())
    except Exception:
        return None


def fetch_status():
    """Fetch /status for controller state."""
    try:
        with urllib.request.urlopen(f"{BASE_URL}/status", timeout=3) as r:
            return json.loads(r.read())
    except Exception:
        return {}


def poll_metrics(csv_writer, stop_event):
    """Background thread: polls /metrics every second, writes to CSV."""
    while not stop_event.is_set():
        tick_start = time.time()
        snap = fetch_metrics()
        status = fetch_status()

        if snap is not None:
            # Skip rows where p95 is null — only write rows with real latency data
            if snap.get("p95") is not None:
                row = {
                    "timestamp": int(time.time() * 1000),
                    "rps":           snap.get("throughput", ""),
                    "p50":           snap.get("p50", ""),
                    "p95":           snap.get("p95", ""),
                    "p99":           snap.get("p99", ""),
                    "cpu":           snap.get("cpu", ""),
                    "memory":        snap.get("memory", ""),
                    "queue_depth":   snap.get("queue_depth", ""),
                    "active_workers":snap.get("active_workers", ""),
                    "max_workers":   status.get("max_workers", ""),
                    "batch_size":    status.get("batch_size", ""),
                }
                csv_writer.writerow(row)

        elapsed = time.time() - tick_start
        sleep_time = max(0, 1.0 - elapsed)
        stop_event.wait(sleep_time)


def run_trial(pattern, adaptive, csv_path):
    """
    Runs one benchmark trial:
    - Starts the server with or without controller
    - Runs k6 load test
    - Polls metrics every second to CSV
    - Returns path to CSV
    """
    print(f"\n{'='*60}")
    print(f"Trial: pattern={pattern}, adaptive={adaptive}")
    print(f"Output: {csv_path}")
    print(f"{'='*60}")

    # Server env
    env = os.environ.copy()
    env["HELIOS_MODEL_INPUT_DIM"] = str(INPUT_DIM)
    env["HELIOS_PORT"] = "8000"
    if not adaptive:
        # Disable controller by setting absurdly long interval
        env["HELIOS_CONTROLLER_INTERVAL"] = "99999"

    # Start server
    print("[benchmark] Starting server...")
    server_proc = subprocess.Popen(
        [SERVER_BINARY],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )

    if not wait_for_server(timeout=30):
        server_proc.kill()
        raise RuntimeError("Server did not become ready within 30s")
    print("[benchmark] Server ready")

    # Open CSV
    csv_file = open(csv_path, "w", newline="")
    writer = csv.DictWriter(csv_file, fieldnames=CSV_FIELDS)
    writer.writeheader()

    # Start metrics polling thread
    stop_event = threading.Event()
    poll_thread = threading.Thread(target=poll_metrics, args=(writer, stop_event), daemon=True)
    poll_thread.start()

    # Run k6
    k6_cmd = [
        "k6", "run",
        "--env", f"BASE_URL={BASE_URL}",
        "--env", f"PATTERN={pattern}",
        "--env", f"INPUT_DIM={INPUT_DIM}",
        "load_test/k6_script.js",
    ]
    print(f"[benchmark] Running k6: {' '.join(k6_cmd)}")
    k6_proc = subprocess.run(k6_cmd, capture_output=False)

    # Stop polling
    stop_event.set()
    poll_thread.join(timeout=3)
    csv_file.flush()
    csv_file.close()

    # Stop server
    server_proc.send_signal(signal.SIGTERM)
    try:
        server_proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        server_proc.kill()

    print(f"[benchmark] Trial complete. CSV rows: {count_csv_rows(csv_path)}")
    return csv_path


def count_csv_rows(path):
    with open(path) as f:
        return sum(1 for _ in f) - 1  # subtract header


def generate_plots(pattern, static_csv, adaptive_csv):
    """Generate comparison PNG plots for a pattern."""
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
        import csv as csv_module
    except ImportError:
        print("[benchmark] matplotlib not available — skipping plots")
        return

    def load_csv(path):
        rows = []
        with open(path) as f:
            reader = csv_module.DictReader(f)
            for row in reader:
                parsed = {}
                for k, v in row.items():
                    try:
                        parsed[k] = float(v) if v != "" else None
                    except ValueError:
                        parsed[k] = v
                rows.append(parsed)
        return rows

    static_rows = load_csv(static_csv)
    adaptive_rows = load_csv(adaptive_csv)

    if not static_rows or not adaptive_rows:
        print(f"[benchmark] No data to plot for {pattern}")
        return

    def extract(rows, key):
        xs = list(range(len(rows)))
        ys = [r.get(key) for r in rows]
        return xs, ys

    fig, axes = plt.subplots(3, 1, figsize=(12, 14))
    fig.suptitle(f"Helios Benchmark — {pattern.upper()} Pattern\nStatic vs Adaptive Config",
                 fontsize=14, fontweight="bold")

    # Plot 1: P95 latency
    ax = axes[0]
    sx, sy = extract(static_rows, "p95")
    ax_x, ay = extract(adaptive_rows, "p95")
    ax.plot(sx, sy, color="#ef4444", label="Static (P95)", linewidth=2)
    ax.plot(ax_x, ay, color="#22c55e", label="Adaptive (P95)", linewidth=2)
    ax.axhline(y=150, color="#f97316", linestyle="--", linewidth=1.5, label="SLO (150ms)")
    ax.set_ylabel("Latency (ms)")
    ax.set_title("P95 Latency")
    ax.legend()
    ax.grid(True, alpha=0.3)

    # Plot 2: CPU usage
    ax = axes[1]
    sx, sy = extract(static_rows, "cpu")
    ax_x, ay = extract(adaptive_rows, "cpu")
    ax.plot(sx, sy, color="#ef4444", label="Static CPU%", linewidth=2)
    ax.plot(ax_x, ay, color="#22c55e", label="Adaptive CPU%", linewidth=2)
    ax.set_ylabel("CPU %")
    ax.set_ylim(0, 100)
    ax.set_title("CPU Usage")
    ax.legend()
    ax.grid(True, alpha=0.3)

    # Plot 3: Queue depth
    ax = axes[2]
    sx, sy = extract(static_rows, "queue_depth")
    ax_x, ay = extract(adaptive_rows, "queue_depth")
    ax.plot(sx, sy, color="#ef4444", label="Static Queue", linewidth=2)
    ax.plot(ax_x, ay, color="#22c55e", label="Adaptive Queue", linewidth=2)
    ax.set_ylabel("Queue Depth")
    ax.set_xlabel("Time (seconds)")
    ax.set_title("Queue Depth")
    ax.legend()
    ax.grid(True, alpha=0.3)

    plt.tight_layout()
    out_path = OUTPUT_DIR / f"{pattern}_comparison.png"
    plt.savefig(out_path, dpi=150, bbox_inches="tight")
    plt.close()
    print(f"[benchmark] Plot saved: {out_path}")


def main():
    print("Helios Benchmark Runner")
    print(f"Patterns: {PATTERNS}")
    print(f"Test duration: {TEST_DURATION} per trial")
    print(f"Output directory: {OUTPUT_DIR}")

    # Check prerequisites
    if not Path(SERVER_BINARY).exists():
        print(f"ERROR: Server binary not found at {SERVER_BINARY}")
        print("Build first: go build -o helios-server ./cmd/server")
        sys.exit(1)

    try:
        subprocess.run(["k6", "version"], capture_output=True, check=True)
    except (subprocess.CalledProcessError, FileNotFoundError):
        print("ERROR: k6 not found. Install from https://k6.io/docs/get-started/installation/")
        sys.exit(1)

    all_results = {}

    for pattern in PATTERNS:
        static_csv = OUTPUT_DIR / f"{pattern}_static.csv"
        adaptive_csv = OUTPUT_DIR / f"{pattern}_adaptive.csv"

        # Static trial first
        run_trial(pattern=pattern, adaptive=False, csv_path=static_csv)
        time.sleep(3)  # Brief pause between trials

        # Adaptive trial
        run_trial(pattern=pattern, adaptive=True, csv_path=adaptive_csv)
        time.sleep(3)

        # Generate comparison plots
        generate_plots(pattern, static_csv, adaptive_csv)

        all_results[pattern] = {
            "static": str(static_csv),
            "adaptive": str(adaptive_csv),
        }

    print("\n" + "="*60)
    print("BENCHMARK COMPLETE")
    print("="*60)
    for pattern, files in all_results.items():
        print(f"\n{pattern.upper()}:")
        print(f"  Static CSV:   {files['static']} ({count_csv_rows(files['static'])} rows)")
        print(f"  Adaptive CSV: {files['adaptive']} ({count_csv_rows(files['adaptive'])} rows)")
        plot = OUTPUT_DIR / f"{pattern}_comparison.png"
        if plot.exists():
            print(f"  Plot:         {plot}")


if __name__ == "__main__":
    main()
