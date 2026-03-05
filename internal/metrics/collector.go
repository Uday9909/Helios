package metrics

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"gonum.org/v1/gonum/stat"
)

// Snapshot is one point-in-time metrics reading.
type Snapshot struct {
	Timestamp   int64    `json:"timestamp"`    // unix ms
	CPU         *float64 `json:"cpu"`          // percent, null until measured
	Memory      *float64 `json:"memory"`       // percent, null until measured
	QueueDepth  int      `json:"queue_depth"`
	ActiveWorkers int    `json:"active_workers"`
	MaxWorkers  int      `json:"max_workers"`
	BatchSize   int      `json:"batch_size"`
	Throughput  float64  `json:"throughput"`   // req/s in last interval
	P50         *float64 `json:"p50"`          // null until sufficient data
	P95         *float64 `json:"p95"`
	P99         *float64 `json:"p99"`
}

// QueueDepthFn and ActiveCountFn are injected after construction
// to avoid circular dependencies.
type QueueDepthFn func() int
type ActiveCountFn func() int
type WorkerStateFn func() (workers int, batchSize int)

// Collector collects and exposes real system metrics.
type Collector struct {
	mu            sync.RWMutex
	latencyWindow []float64   // last 1000 real latencies in ms
	snapshots     []Snapshot  // last 300 snapshots
	latest        *Snapshot   // most recent, nil until first tick

	completedLastSecond atomic.Int64

	getQueueDepth  QueueDepthFn
	getActiveCount ActiveCountFn
	getWorkerState WorkerStateFn

	stop chan struct{}
}

// New creates a Collector. Call SetReferences before Start.
func New() *Collector {
	return &Collector{
		latencyWindow: make([]float64, 0, 1000),
		snapshots:     make([]Snapshot, 0, 300),
		stop:          make(chan struct{}),
	}
}

// SetReferences injects live system references (called after all components constructed).
func (c *Collector) SetReferences(qd QueueDepthFn, ac ActiveCountFn, ws WorkerStateFn) {
	c.getQueueDepth = qd
	c.getActiveCount = ac
	c.getWorkerState = ws
}

// Start begins the background 1-second tick.
func (c *Collector) Start() {
	go c.loop()
}

// Stop shuts down the background goroutine.
func (c *Collector) Stop() {
	close(c.stop)
}

func (c *Collector) loop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.tick()
		case <-c.stop:
			return
		}
	}
}

func (c *Collector) tick() {
	// CPU — non-blocking, uses delta since last call
	cpuPercents, err := cpu.Percent(0, false)
	var cpuVal *float64
	if err == nil && len(cpuPercents) > 0 {
		v := cpuPercents[0]
		cpuVal = &v
	}

	// Memory
	var memVal *float64
	vmStat, err := mem.VirtualMemory()
	if err == nil {
		v := vmStat.UsedPercent
		memVal = &v
	}

	// Queue depth and worker state
	queueDepth := 0
	activeWorkers := 0
	maxWorkers := 0
	batchSize := 0
	if c.getQueueDepth != nil {
		queueDepth = c.getQueueDepth()
	}
	if c.getActiveCount != nil {
		activeWorkers = c.getActiveCount()
	}
	if c.getWorkerState != nil {
		maxWorkers, batchSize = c.getWorkerState()
	}

	// Throughput
	completed := c.completedLastSecond.Swap(0)
	throughput := float64(completed)

	// Latency percentiles — only if we have real data
	c.mu.RLock()
	window := make([]float64, len(c.latencyWindow))
	copy(window, c.latencyWindow)
	c.mu.RUnlock()

	var p50, p95, p99 *float64
	if len(window) >= 1 {
		sorted := make([]float64, len(window))
		copy(sorted, window)
		// sort inline for percentile computation
		sortFloat64s(sorted)

		v50 := stat.Quantile(0.50, stat.Empirical, sorted, nil)
		v95 := stat.Quantile(0.95, stat.Empirical, sorted, nil)
		v99 := stat.Quantile(0.99, stat.Empirical, sorted, nil)
		p50 = &v50
		p95 = &v95
		p99 = &v99
	}

	snap := Snapshot{
		Timestamp:     time.Now().UnixMilli(),
		CPU:           cpuVal,
		Memory:        memVal,
		QueueDepth:    queueDepth,
		ActiveWorkers: activeWorkers,
		MaxWorkers:    maxWorkers,
		BatchSize:     batchSize,
		Throughput:    throughput,
		P50:           p50,
		P95:           p95,
		P99:           p99,
	}

	c.mu.Lock()
	c.latest = &snap
	if len(c.snapshots) >= 300 {
		c.snapshots = c.snapshots[1:]
	}
	c.snapshots = append(c.snapshots, snap)
	c.mu.Unlock()
}

// RecordLatency records a real inference latency in milliseconds.
func (c *Collector) RecordLatency(ms float64) {
	c.mu.Lock()
	if len(c.latencyWindow) >= 1000 {
		c.latencyWindow = c.latencyWindow[1:]
	}
	c.latencyWindow = append(c.latencyWindow, ms)
	c.mu.Unlock()
	c.completedLastSecond.Add(1)
}

// GetSnapshot returns the latest snapshot, or nil if none yet.
func (c *Collector) GetSnapshot() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

// GetHistory returns all stored snapshots.
func (c *Collector) GetHistory() []Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]Snapshot, len(c.snapshots))
	copy(result, c.snapshots)
	return result
}

// sortFloat64s sorts in place (insertion sort — fine for <=1000 elements).
func sortFloat64s(a []float64) {
	for i := 1; i < len(a); i++ {
		key := a[i]
		j := i - 1
		for j >= 0 && a[j] > key {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = key
	}
}
