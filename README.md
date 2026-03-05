# Helios — Adaptive CPU-Only Inference Serving Platform

A production-grade, self-regulating ML inference server written in Go.
Dynamically adjusts concurrency and batching to maintain P95 latency below a
configurable SLO. Runs entirely on CPU with a live React dashboard.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                        Client(s)                            │
└─────────────────────────────┬───────────────────────────────┘
                              │ POST /predict
                              ▼
┌─────────────────────────────────────────────────────────────┐
│              Gin HTTP Server  (cmd/server/main.go)          │
│  • Validates input dimensions                               │
│  • Attaches request_id, enqueue_time                        │
│  • Enqueues — NEVER runs inference in request goroutine     │
│  • Blocks on done channel (10s timeout)                     │
└─────────────────────────────┬───────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│           Priority Scheduler  (internal/scheduler)          │
│  • premium chan (buffered 40% of max_queue_size)            │
│  • standard chan (buffered 60% of max_queue_size)           │
│  • Weighted Fair Scheduling: 3 premium → 1 standard        │
│  • admission_control atomic bool (set by controller)        │
│  • Returns 429 when queue full or AC active for standard    │
└─────────────────────────────┬───────────────────────────────┘
                              │ Dequeue()
                              ▼
┌─────────────────────────────────────────────────────────────┐
│            Worker Pool  (internal/worker)                   │
│  • Semaphore chan controls real concurrency limit            │
│  • Dispatch goroutine: Dequeue → acquire sem → go execute() │
│  • execute(): RunInference() → RecordLatency() → done       │
│  • SetMaxWorkers() rebuilds semaphore, drains in-flight      │
└─────────────────────────────┬───────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│              Model  (internal/model)                        │
│  • Y = X @ W + B  via gonum/mat                            │
│  • Weights generated on first run, saved to server/         │
│  • Returns (output []float64, latency_ms float64)           │
│  • Latency = real wall-clock time of matrix multiply        │
└─────────────────────────────┬───────────────────────────────┘
                              │ RecordLatency()
                              ▼
┌─────────────────────────────────────────────────────────────┐
│           Metrics Collector  (internal/metrics)             │
│  • deque(1000) of real latency samples                      │
│  • 1s background tick: psutil CPU/memory, queue depth       │
│  • Percentiles via gonum stat.Quantile                      │
│  • Returns nil for all percentiles until data exists        │
│  • 300-snapshot rolling history for dashboard               │
└─────────────────────────────┬───────────────────────────────┘
                              │ GetSnapshot() every 2s
                              ▼
┌─────────────────────────────────────────────────────────────┐
│          Adaptive Controller  (internal/controller)         │
│  • Case 1: P95 > SLO ∧ CPU > 85% → ↓workers, ↓batch       │
│  • Case 2: P95 > SLO ∧ queue growing → admission_control=ON│
│  • Case 3: P95 < SLO×0.7 ∧ CPU < 60% → ↑workers, AC=OFF   │
│  • Case 4: memory > 85% → ↓batch                           │
│  • Skips tick if P95 or CPU is nil (no fake decisions)      │
└─────────────────────────────────────────────────────────────┘
```

---

## Prerequisites

| Tool | Version | Install |
|------|---------|---------|
| Go | 1.22+ | https://go.dev/dl/ |
| Node.js | 18+ | https://nodejs.org |
| k6 | latest | https://k6.io/docs/get-started/installation/ |
| Python | 3.9+ (benchmarks only) | https://python.org |

---

## Setup & Run

### 1. Install Go dependencies

```bash
cd helios
go mod tidy
```

### 2. Build the server

```bash
go build -o helios-server ./cmd/server
```

### 3. Run the server

```bash
./helios-server
```

Server starts on `:8000`. On first run, model weights are generated and saved to
`server/model.weights`. Subsequent starts load them instantly.

### 4. Run the dashboard

```bash
cd dashboard
npm install
npm run dev
```

Open http://localhost:5173

The dashboard shows **"Waiting for data"** for all metrics until real requests
have been served. This is correct behavior.

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `HELIOS_PORT` | `8000` | HTTP listen port |
| `HELIOS_SLO_MS` | `150` | P95 latency SLO in milliseconds |
| `HELIOS_MAX_WORKERS` | `cpu_count` | Initial worker concurrency limit |
| `HELIOS_BATCH_SIZE` | `8` | Initial batch size |
| `HELIOS_MAX_QUEUE_SIZE` | `500` | Total queue capacity (premium + standard) |
| `HELIOS_CONTROLLER_INTERVAL` | `2` | Controller tick interval in seconds |
| `HELIOS_MEMORY_THRESHOLD` | `85` | Memory % that triggers batch reduction |
| `HELIOS_MODEL_INPUT_DIM` | `128` | Input vector dimension |

---

## API Reference

### `POST /predict`

Run inference on a batch of inputs.

**Request:**
```json
{
  "input": [[0.1, 0.2, ..., 0.128]],
  "priority": "premium"
}
```
- `input`: array of rows, each row must have exactly `HELIOS_MODEL_INPUT_DIM` values
- `priority`: `"premium"` or `"standard"`

**Response 200:**
```json
{
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "output": [0.347],
  "queue_wait_ms": 2.4,
  "inference_ms": 0.8
}
```

**Response 429 (overloaded):**
```json
{
  "error": "overloaded",
  "reason": "queue_full"
}
```
Possible reasons: `queue_full`, `admission_control`

**Response 504:**
```json
{
  "error": "timeout",
  "reason": "inference did not complete within 10s"
}
```

---

### `GET /metrics`

Current system metrics snapshot.

```json
{
  "timestamp": 1721000000000,
  "p50": 0.82,
  "p95": 1.14,
  "p99": 2.31,
  "cpu": 34.2,
  "memory": 41.8,
  "queue_depth": 3,
  "active_workers": 2,
  "max_workers": 4,
  "batch_size": 8,
  "throughput": 47.0
}
```

**On fresh start** (no requests yet), latency percentiles are `null`:
```json
{
  "p50": null, "p95": null, "p99": null,
  "cpu": null, "memory": null,
  "note": "no_data_yet"
}
```

---

### `GET /metrics/history`

Returns up to 300 snapshots (5 minutes) as a JSON array.
Same schema as `/metrics`. Used by the dashboard.

---

### `GET /status`

Controller state:
```json
{
  "max_workers": 4,
  "batch_size": 8,
  "admission_control": false,
  "slo_ms": 150.0,
  "last_action": "[CASE3] p95=12.1ms < SLO*0.7=105ms, cpu=18.3% < 60% → workers 3→4",
  "last_action_time": "2024-07-15T10:23:45Z"
}
```

---

### `GET /health`

```json
{"status": "ok"}
```

---

### `POST /simulate`

Inject failure scenarios for testing.

```json
{
  "scenario": "delay",
  "duration_seconds": 10
}
```

| Scenario | Effect |
|----------|--------|
| `delay` | Injects 300ms sleep into inference execution |
| `cpu_stress` | Spins goroutines on all cores |
| `memory_spike` | Allocates ~500MB for N seconds |
| `worker_crash` | Next 3 inference calls return errors |

---

## Load Testing

```bash
# Steady load (50 rps, 60 seconds)
k6 run --env PATTERN=steady --env INPUT_DIM=128 load_test/k6_script.js

# Burst load (20 → 150 → 20 rps)
k6 run --env PATTERN=burst --env INPUT_DIM=128 load_test/k6_script.js

# Long tail (exponential inter-arrival)
k6 run --env PATTERN=longtail --env INPUT_DIM=128 load_test/k6_script.js
```

---

## Benchmarks

```bash
# Install Python deps
pip install matplotlib

# Build server first
go build -o helios-server ./cmd/server

# Run all patterns (static vs adaptive)
python benchmarks/run_benchmark.py
```

Outputs:
- `benchmarks/steady_static.csv` / `benchmarks/steady_adaptive.csv`
- `benchmarks/burst_static.csv` / `benchmarks/burst_adaptive.csv`
- `benchmarks/longtail_static.csv` / `benchmarks/longtail_adaptive.csv`
- `benchmarks/steady_comparison.png`
- `benchmarks/burst_comparison.png`
- `benchmarks/longtail_comparison.png`

Each CSV row is one real metrics poll during a real load test run.
Rows where P95 is null are excluded — no synthetic data.

---

## Verification Checklist

Run these checks in order. All must pass.

```bash
# 1. Start server
./helios-server

# 2. Null check (latency percentiles must be null on fresh start)
curl http://localhost:8000/metrics | python3 -m json.tool
# Expect: "p95": null, "p50": null

# 3. Health
curl http://localhost:8000/health
# Expect: {"status":"ok"}

# 4. Real inference (use your actual input_dim)
curl -s -X POST http://localhost:8000/predict \
  -H 'Content-Type: application/json' \
  -d "{\"input\": [$(python3 -c 'print("["+",".join(["0.1"]*128)+"]")')], \"priority\": \"standard\"}" \
  | python3 -m json.tool
# Expect: output array, inference_ms > 0

# 5. P50 appears after first request
curl http://localhost:8000/metrics | python3 -m json.tool
# Expect: "p50": <real number>, not null

# 6. Queue overflow test
for i in $(seq 1 510); do
  curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8000/predict \
    -H 'Content-Type: application/json' \
    -d "{\"input\": [$(python3 -c 'print("["+",".join(["0.1"]*128)+"]")')], \"priority\": \"standard\"}" &
done
wait
# Expect: mix of 200 and 429 responses

# 7. Run load test and watch controller logs
k6 run --env PATTERN=steady --env INPUT_DIM=128 load_test/k6_script.js
# Expect: server logs show [Controller] lines with case labels and parameter changes
```

---

## Project Structure

```
helios/
├── cmd/
│   └── server/
│       └── main.go          # Entry point, component wiring, graceful shutdown
├── internal/
│   ├── model/
│   │   └── model.go         # Linear layer: Y = X @ W + B via gonum
│   ├── metrics/
│   │   └── collector.go     # Real psutil metrics, null-safe percentiles
│   ├── scheduler/
│   │   └── scheduler.go     # WFS priority queues, admission control
│   ├── worker/
│   │   └── pool.go          # Semaphore-based pool, dynamic resize
│   ├── controller/
│   │   └── controller.go    # Feedback control loop, 4 cases
│   └── api/
│       └── handlers.go      # Gin HTTP handlers, failure simulation
├── load_test/
│   └── k6_script.js         # Three traffic patterns
├── benchmarks/
│   └── run_benchmark.py     # Automated benchmarking + matplotlib plots
├── dashboard/
│   ├── src/
│   │   ├── App.jsx          # Live charts, null-safe rendering
│   │   └── main.jsx
│   ├── index.html
│   ├── package.json
│   └── vite.config.js
├── go.mod
└── README.md
```

---

## Design Decisions

**Why Go instead of Python?**
- True parallelism: goroutines are not limited by a GIL. `max_workers=4` means 4 threads actually running matrix operations simultaneously.
- The semaphore pattern for concurrency control is idiomatic Go and exactly maps to our worker pool semantics.
- `atomic.Bool` for admission control is lock-free, so the hot path (every enqueue) has zero contention.

**Why gonum instead of ONNX?**
- `onnxruntime_go` requires linking a native C library which breaks portability.
- `gonum` is pure Go, compiles everywhere, and the linear layer math is identical.
- The weights are real (Xavier-initialized random values), saved to disk, and loaded on restart — not generated per-request.

**Why null instead of 0 for missing metrics?**
- Zero is a valid latency value. Returning 0 before any requests have completed would be indistinguishable from a system with zero latency.
- `null` propagates honestly through the JSON → dashboard pipeline, triggering "Waiting for data" UI states instead of misleading flat lines.

**Why a semaphore instead of a goroutine pool?**
- Goroutine pools in Go are an anti-pattern. Goroutines are cheap. The bottleneck is CPU, not goroutine overhead.
- A buffered channel as a semaphore is the idiomatic Go pattern and directly maps to our concurrency limit semantics.
- `SetMaxWorkers()` rebuilds the semaphore, which means in-flight goroutines drain naturally and new ones are gated by the new limit.
