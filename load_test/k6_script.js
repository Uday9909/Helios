import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

// ── Custom metrics ────────────────────────────────────────────────────────────
const rejectedRequests = new Counter('rejected_requests');
const inferenceLatency = new Trend('inference_latency_ms', true);
const queueWait = new Trend('queue_wait_ms', true);
const successRate = new Rate('success_rate');

// ── Config ────────────────────────────────────────────────────────────────────
const BASE_URL = __ENV.BASE_URL || 'http://localhost:8000';
const INPUT_DIM = parseInt(__ENV.INPUT_DIM || '128');

// Traffic pattern selected via PATTERN env var: steady | burst | longtail
const PATTERN = __ENV.PATTERN || 'steady';

// ── Scenarios ─────────────────────────────────────────────────────────────────
export const options = {
  scenarios: {
    traffic: PATTERN === 'steady' ? steadyScenario() :
             PATTERN === 'burst'  ? burstScenario() :
                                    longtailScenario(),
  },
  thresholds: {
    // These are measurement thresholds — NOT pass/fail gates
    // We want to observe what happens, not hide failures
    'http_req_duration': ['p(95)<5000'], // Only fail k6 on extreme timeouts
  },
};

function steadyScenario() {
  return {
    executor: 'constant-arrival-rate',
    rate: 50,
    timeUnit: '1s',
    duration: '60s',
    preAllocatedVUs: 80,
    maxVUs: 150,
  };
}

function burstScenario() {
  return {
    executor: 'ramping-arrival-rate',
    startRate: 20,
    timeUnit: '1s',
    preAllocatedVUs: 200,
    maxVUs: 300,
    stages: [
      { duration: '30s', target: 150 }, // ramp up 20 → 150
      { duration: '30s', target: 150 }, // hold at 150
      { duration: '30s', target: 20  }, // ramp down 150 → 20
    ],
  };
}

function longtailScenario() {
  return {
    executor: 'constant-vus',
    vus: 40,
    duration: '60s',
  };
}

// ── Build a real input payload ─────────────────────────────────────────────────
function buildInput(priority) {
  const row = Array.from({ length: INPUT_DIM }, () => Math.random() * 2 - 1);
  return {
    input: [row],
    priority: priority,
  };
}

// ── Main VU function ──────────────────────────────────────────────────────────
export default function () {
  // Priority distribution: 30% premium, 70% standard
  const priority = Math.random() < 0.3 ? 'premium' : 'standard';

  // Long tail: exponential inter-arrival time (mean 1/40 sec = 25ms)
  if (PATTERN === 'longtail') {
    const waitMs = -Math.log(Math.random()) * (1000 / 40);
    sleep(waitMs / 1000);
  }

  const payload = JSON.stringify(buildInput(priority));
  const params = {
    headers: { 'Content-Type': 'application/json' },
    timeout: '12s',
  };

  const res = http.post(`${BASE_URL}/predict`, payload, params);

  if (res.status === 200) {
    successRate.add(1);
    const body = JSON.parse(res.body);
    if (body.inference_ms !== undefined) {
      inferenceLatency.add(body.inference_ms);
    }
    if (body.queue_wait_ms !== undefined) {
      queueWait.add(body.queue_wait_ms);
    }
    check(res, {
      'has output': (r) => JSON.parse(r.body).output !== undefined,
      'has request_id': (r) => JSON.parse(r.body).request_id !== undefined,
    });
  } else if (res.status === 429) {
    // 429 is expected behavior under load — record but don't fail
    rejectedRequests.add(1);
    successRate.add(0);
  } else {
    successRate.add(0);
    console.error(`Unexpected status ${res.status}: ${res.body}`);
  }
}

// ── Setup: verify server is reachable before test ─────────────────────────────
export function setup() {
  const res = http.get(`${BASE_URL}/health`);
  if (res.status !== 200) {
    throw new Error(`Server not ready: ${res.status} ${res.body}`);
  }
  console.log(`[k6] Server ready at ${BASE_URL}`);
  console.log(`[k6] Pattern: ${PATTERN}, InputDim: ${INPUT_DIM}`);
}

// ── Teardown: log final metrics summary ───────────────────────────────────────
export function teardown() {
  const res = http.get(`${BASE_URL}/metrics`);
  if (res.status === 200) {
    const snap = JSON.parse(res.body);
    console.log(`[k6] Final server metrics:`);
    console.log(`  P95: ${snap.p95}ms`);
    console.log(`  CPU: ${snap.cpu}%`);
    console.log(`  Queue depth: ${snap.queue_depth}`);
    console.log(`  Active workers: ${snap.active_workers}`);
  }
}
