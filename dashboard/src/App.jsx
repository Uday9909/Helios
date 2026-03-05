import { useState, useEffect, useCallback, useRef } from 'react'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip,
  ResponsiveContainer, ReferenceLine, Legend
} from 'recharts'

// ── Constants ─────────────────────────────────────────────────────────────────
const SLO_MS = 150
const POLL_INTERVAL_MS = 2000
const MAX_CHART_POINTS = 120  // 4 minutes of history at 2s resolution

// ── Utility: format timestamp for X axis ─────────────────────────────────────
function fmtTime(tsMs) {
  if (!tsMs) return ''
  const d = new Date(tsMs)
  return `${d.getMinutes().toString().padStart(2,'0')}:${d.getSeconds().toString().padStart(2,'0')}`
}

function fmt1(v) {
  return v != null ? v.toFixed(1) : null
}

// ── Waiting card shown when data is not yet available ─────────────────────────
function WaitingCard({ label }) {
  return (
    <div style={styles.waitingCard}>
      <div style={styles.waitingLabel}>{label}</div>
      <div style={styles.waitingText}>Waiting for data</div>
    </div>
  )
}

// ── Stat card shown when data IS available ────────────────────────────────────
function StatCard({ label, value, unit, highlight }) {
  return (
    <div style={{ ...styles.statCard, ...(highlight ? styles.statCardHighlight : {}) }}>
      <div style={styles.statLabel}>{label}</div>
      <div style={styles.statValue}>
        {value}
        {unit && <span style={styles.statUnit}>{unit}</span>}
      </div>
    </div>
  )
}

// ── Admission control badge ───────────────────────────────────────────────────
function AdmissionBadge({ active }) {
  return (
    <div style={styles.statCard}>
      <div style={styles.statLabel}>Admission Control</div>
      <div style={{
        ...styles.badge,
        background: active ? '#7f1d1d' : '#14532d',
        color: active ? '#fca5a5' : '#86efac',
      }}>
        {active ? '⚠ ON — shedding standard' : '✓ OFF'}
      </div>
    </div>
  )
}

// ── Chart wrapper: renders nothing until data is non-null ─────────────────────
function LiveChart({ title, data, lines, yDomain, yLabel, sloLine }) {
  // Filter to only points that have at least one non-null value for our keys
  const lineKeys = lines.map(l => l.key)
  const validData = data.filter(d => lineKeys.some(k => d[k] != null))

  if (validData.length === 0) {
    return <WaitingCard label={title} />
  }

  return (
    <div style={styles.chartCard}>
      <div style={styles.chartTitle}>{title}</div>
      <ResponsiveContainer width="100%" height={220}>
        <LineChart data={validData} margin={{ top: 8, right: 24, left: 0, bottom: 0 }}>
          <CartesianGrid strokeDasharray="3 3" stroke="#2a2a2a" />
          <XAxis
            dataKey="ts"
            tickFormatter={fmtTime}
            tick={{ fill: '#888', fontSize: 11 }}
            interval="preserveStartEnd"
          />
          <YAxis
            domain={yDomain || ['auto', 'auto']}
            tick={{ fill: '#888', fontSize: 11 }}
            label={yLabel ? { value: yLabel, angle: -90, position: 'insideLeft', fill: '#666', fontSize: 11 } : undefined}
          />
          <Tooltip
            contentStyle={{ background: '#1a1a1a', border: '1px solid #333', borderRadius: 8 }}
            labelStyle={{ color: '#aaa', fontSize: 11 }}
            formatter={(v, name) => [v != null ? v.toFixed(2) : 'N/A', name]}
            labelFormatter={fmtTime}
          />
          <Legend wrapperStyle={{ color: '#aaa', fontSize: 12 }} />
          {sloLine && (
            <ReferenceLine
              y={SLO_MS}
              stroke="#ef4444"
              strokeDasharray="6 3"
              label={{ value: `SLO ${SLO_MS}ms`, fill: '#ef4444', fontSize: 11, position: 'right' }}
            />
          )}
          {lines.map(l => (
            <Line
              key={l.key}
              type="monotone"
              dataKey={l.key}
              name={l.name}
              stroke={l.color}
              strokeWidth={2}
              dot={false}
              connectNulls={false}
              isAnimationActive={false}
            />
          ))}
        </LineChart>
      </ResponsiveContainer>
    </div>
  )
}

// ── Main App ──────────────────────────────────────────────────────────────────
export default function App() {
  // chartData: array of flattened point objects for Recharts
  const [chartData, setChartData] = useState([])
  // status: controller state from /status
  const [status, setStatus] = useState(null)
  // latest: most recent snapshot
  const [latest, setLatest] = useState(null)
  // connection state
  const [connected, setConnected] = useState(false)
  const [lastPoll, setLastPoll] = useState(null)

  const historyRef = useRef([])

  const poll = useCallback(async () => {
    try {
      // Fetch history and status in parallel
      const [histRes, statusRes] = await Promise.all([
        fetch('/metrics/history'),
        fetch('/status'),
      ])

      if (!histRes.ok || !statusRes.ok) {
        setConnected(false)
        return
      }

      const history = await histRes.json()
      const statusData = await statusRes.json()

      setConnected(true)
      setLastPoll(Date.now())
      setStatus(statusData)

      if (!Array.isArray(history) || history.length === 0) return

      // Build chart-friendly objects — only include fields that are non-null
      const points = history.map(snap => {
        const pt = { ts: snap.timestamp }
        if (snap.p50   != null) pt.p50   = snap.p50
        if (snap.p95   != null) pt.p95   = snap.p95
        if (snap.p99   != null) pt.p99   = snap.p99
        if (snap.cpu   != null) pt.cpu   = snap.cpu
        if (snap.memory!= null) pt.mem   = snap.memory
        if (snap.queue_depth != null) pt.queue = snap.queue_depth
        if (snap.throughput  != null) pt.rps   = snap.throughput
        return pt
      })

      // Keep only the last MAX_CHART_POINTS
      const trimmed = points.slice(-MAX_CHART_POINTS)
      setChartData(trimmed)

      // Latest snapshot
      const last = history[history.length - 1]
      setLatest(last)

    } catch {
      setConnected(false)
    }
  }, [])

  useEffect(() => {
    poll()
    const id = setInterval(poll, POLL_INTERVAL_MS)
    return () => clearInterval(id)
  }, [poll])

  // ── Render ──────────────────────────────────────────────────────────────────
  return (
    <div style={styles.root}>
      {/* Header */}
      <div style={styles.header}>
        <div>
          <div style={styles.headerTitle}>⚡ Helios</div>
          <div style={styles.headerSub}>Adaptive Inference Monitor</div>
        </div>
        <div style={styles.headerRight}>
          <div style={{
            ...styles.connDot,
            background: connected ? '#22c55e' : '#ef4444'
          }} />
          <span style={styles.connLabel}>
            {connected ? 'Live' : 'Disconnected'}
          </span>
          {lastPoll && (
            <span style={styles.pollTime}>
              Updated {new Date(lastPoll).toLocaleTimeString()}
            </span>
          )}
        </div>
      </div>

      {/* Stat cards row — only render cards with real data */}
      <div style={styles.statsRow}>
        {latest?.throughput != null
          ? <StatCard label="Throughput" value={fmt1(latest.throughput)} unit=" req/s" />
          : <WaitingCard label="Throughput" />
        }
        {status?.max_workers != null
          ? <StatCard
              label="Active Workers"
              value={`${latest?.active_workers ?? '?'} / ${status.max_workers}`}
            />
          : <WaitingCard label="Active Workers" />
        }
        {status?.batch_size != null
          ? <StatCard label="Batch Size" value={status.batch_size} />
          : <WaitingCard label="Batch Size" />
        }
        {latest?.p95 != null
          ? <StatCard
              label="P95 Latency"
              value={fmt1(latest.p95)}
              unit=" ms"
              highlight={latest.p95 > SLO_MS}
            />
          : <WaitingCard label="P95 Latency" />
        }
        {status != null
          ? <AdmissionBadge active={status.admission_control} />
          : <WaitingCard label="Admission Control" />
        }
      </div>

      {/* Controller last action */}
      {status?.last_action && (
        <div style={styles.actionBar}>
          <span style={styles.actionLabel}>Last controller action: </span>
          <span style={styles.actionText}>{status.last_action}</span>
          {status.last_action_time && (
            <span style={styles.actionTime}> at {status.last_action_time}</span>
          )}
        </div>
      )}

      {/* Charts grid */}
      <div style={styles.chartsGrid}>
        <LiveChart
          title="Latency (P50 / P95)"
          data={chartData}
          lines={[
            { key: 'p95', name: 'P95', color: '#f97316' },
            { key: 'p50', name: 'P50', color: '#6366f1' },
          ]}
          yLabel="ms"
          sloLine
        />
        <LiveChart
          title="CPU & Memory"
          data={chartData}
          lines={[
            { key: 'cpu', name: 'CPU %',    color: '#ef4444' },
            { key: 'mem', name: 'Memory %', color: '#a855f7' },
          ]}
          yDomain={[0, 100]}
          yLabel="%"
        />
        <LiveChart
          title="Queue Depth"
          data={chartData}
          lines={[
            { key: 'queue', name: 'Queue', color: '#eab308' },
          ]}
          yLabel="requests"
        />
        <LiveChart
          title="Throughput"
          data={chartData}
          lines={[
            { key: 'rps', name: 'req/s', color: '#22c55e' },
          ]}
          yLabel="req/s"
        />
      </div>

      {/* Footer */}
      <div style={styles.footer}>
        SLO target: P95 &lt; {SLO_MS}ms &nbsp;·&nbsp;
        Polling every {POLL_INTERVAL_MS/1000}s &nbsp;·&nbsp;
        All data sourced from live server metrics
      </div>
    </div>
  )
}

// ── Styles ────────────────────────────────────────────────────────────────────
const styles = {
  root: {
    minHeight: '100vh',
    background: '#0a0a0a',
    color: '#e5e5e5',
    fontFamily: "'Inter', system-ui, sans-serif",
    padding: '0 0 40px',
  },
  header: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    padding: '20px 32px',
    borderBottom: '1px solid #1f1f1f',
    background: '#111',
  },
  headerTitle: {
    fontSize: 22,
    fontWeight: 700,
    letterSpacing: '-0.5px',
    color: '#fff',
  },
  headerSub: {
    fontSize: 12,
    color: '#666',
    marginTop: 2,
  },
  headerRight: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  connDot: {
    width: 8,
    height: 8,
    borderRadius: '50%',
  },
  connLabel: {
    fontSize: 13,
    color: '#aaa',
  },
  pollTime: {
    fontSize: 11,
    color: '#555',
    marginLeft: 8,
  },
  statsRow: {
    display: 'flex',
    gap: 12,
    padding: '20px 32px 0',
    flexWrap: 'wrap',
  },
  statCard: {
    background: '#141414',
    border: '1px solid #222',
    borderRadius: 10,
    padding: '14px 20px',
    minWidth: 140,
    flex: '1 1 140px',
  },
  statCardHighlight: {
    borderColor: '#7f1d1d',
    background: '#1a0a0a',
  },
  statLabel: {
    fontSize: 11,
    color: '#666',
    textTransform: 'uppercase',
    letterSpacing: '0.05em',
    marginBottom: 6,
  },
  statValue: {
    fontSize: 26,
    fontWeight: 700,
    color: '#fff',
    letterSpacing: '-0.5px',
  },
  statUnit: {
    fontSize: 13,
    color: '#888',
    fontWeight: 400,
  },
  badge: {
    display: 'inline-block',
    padding: '6px 12px',
    borderRadius: 6,
    fontSize: 13,
    fontWeight: 600,
    marginTop: 4,
  },
  waitingCard: {
    background: '#111',
    border: '1px solid #1f1f1f',
    borderRadius: 10,
    padding: '14px 20px',
    minWidth: 140,
    flex: '1 1 140px',
  },
  waitingLabel: {
    fontSize: 11,
    color: '#444',
    textTransform: 'uppercase',
    letterSpacing: '0.05em',
    marginBottom: 6,
  },
  waitingText: {
    fontSize: 13,
    color: '#444',
    fontStyle: 'italic',
  },
  actionBar: {
    margin: '16px 32px 0',
    padding: '10px 16px',
    background: '#0f1117',
    border: '1px solid #1e293b',
    borderRadius: 8,
    fontSize: 12,
  },
  actionLabel: {
    color: '#64748b',
  },
  actionText: {
    color: '#94a3b8',
  },
  actionTime: {
    color: '#475569',
  },
  chartsGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(2, 1fr)',
    gap: 16,
    padding: '20px 32px 0',
  },
  chartCard: {
    background: '#111',
    border: '1px solid #1f1f1f',
    borderRadius: 12,
    padding: '16px 12px 8px',
  },
  chartTitle: {
    fontSize: 13,
    color: '#888',
    marginBottom: 12,
    paddingLeft: 8,
    fontWeight: 500,
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
  },
  footer: {
    textAlign: 'center',
    fontSize: 11,
    color: '#444',
    marginTop: 32,
    padding: '0 32px',
  },
}
