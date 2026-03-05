package controller

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/helios/internal/metrics"
	"github.com/helios/internal/scheduler"
	"github.com/helios/internal/worker"
)

// AdaptiveController is a feedback control loop that adjusts system parameters
// to maintain P95 latency below the configured SLO.
type AdaptiveController struct {
	collector  *metrics.Collector
	pool       *worker.Pool
	sched      *scheduler.Scheduler

	sloMs    float64
	interval time.Duration
	maxCores int

	mu     sync.RWMutex
	state  State

	prevQueueDepth int
	stop           chan struct{}
}

// State is the current controller configuration.
type State struct {
	MaxWorkers       int     `json:"max_workers"`
	BatchSize        int     `json:"batch_size"`
	AdmissionControl bool    `json:"admission_control"`
	SloMs            float64 `json:"slo_ms"`
	LastAction       string  `json:"last_action"`
	LastActionTime   string  `json:"last_action_time"`
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// New creates a controller. Call Start() to begin the control loop.
func New(collector *metrics.Collector, pool *worker.Pool, sched *scheduler.Scheduler) *AdaptiveController {
	sloMs := envFloat("HELIOS_SLO_MS", 150.0)
	intervalSec := envFloat("HELIOS_CONTROLLER_INTERVAL", 2.0)
	maxWorkers := envInt("HELIOS_MAX_WORKERS", runtime.NumCPU())
	batchSize := envInt("HELIOS_BATCH_SIZE", 8)

	return &AdaptiveController{
		collector: collector,
		pool:      pool,
		sched:     sched,
		sloMs:     sloMs,
		interval:  time.Duration(intervalSec * float64(time.Second)),
		maxCores:  runtime.NumCPU(),
		state: State{
			MaxWorkers: maxWorkers,
			BatchSize:  batchSize,
			SloMs:      sloMs,
		},
		stop: make(chan struct{}),
	}
}

// Start begins the control loop goroutine.
func (c *AdaptiveController) Start() {
	go c.loop()
}

// Stop shuts down the control loop.
func (c *AdaptiveController) Stop() {
	close(c.stop)
}

func (c *AdaptiveController) loop() {
	ticker := time.NewTicker(c.interval)
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

func (c *AdaptiveController) tick() {
	snap := c.collector.GetSnapshot()

	// Do not act without real data
	if snap == nil || snap.P95 == nil || snap.CPU == nil {
		return
	}

	p95 := *snap.P95
	cpuPct := *snap.CPU
	memPct := 0.0
	if snap.Memory != nil {
		memPct = *snap.Memory
	}
	queueDepth := snap.QueueDepth

	c.mu.Lock()
	currentWorkers := c.state.MaxWorkers
	currentBatch := c.state.BatchSize
	prevQD := c.prevQueueDepth
	c.prevQueueDepth = queueDepth
	c.mu.Unlock()

	memThreshold := envFloat("HELIOS_MEMORY_THRESHOLD", 85.0)

	// --- Four control cases ---

	// Case 1: Latency too high AND CPU saturated → reduce concurrency
	if p95 > c.sloMs && cpuPct > 85 {
		newWorkers := max(1, currentWorkers-1)
		newBatch := max(1, currentBatch-1)
		c.pool.SetMaxWorkers(newWorkers)
		c.pool.SetBatchSize(newBatch)
		c.recordAction("CASE1", fmt.Sprintf(
			"p95=%.1fms > SLO=%.0fms, cpu=%.1f%% > 85%% → workers %d→%d, batch %d→%d",
			p95, c.sloMs, cpuPct, currentWorkers, newWorkers, currentBatch, newBatch,
		))
		c.mu.Lock()
		c.state.MaxWorkers = newWorkers
		c.state.BatchSize = newBatch
		c.mu.Unlock()
		return
	}

	// Case 2: Latency too high AND queue is growing → enable admission control
	if p95 > c.sloMs && queueDepth > prevQD {
		c.sched.AdmissionControl.Store(true)
		c.recordAction("CASE2", fmt.Sprintf(
			"p95=%.1fms > SLO=%.0fms, queue growing %d→%d → admission_control=ON",
			p95, c.sloMs, prevQD, queueDepth,
		))
		c.mu.Lock()
		c.state.AdmissionControl = true
		c.mu.Unlock()
		return
	}

	// Case 3: Latency well below SLO AND CPU has headroom → increase concurrency
	if p95 < c.sloMs*0.7 && cpuPct < 60 {
		newWorkers := min(c.maxCores, currentWorkers+1)
		c.pool.SetMaxWorkers(newWorkers)
		c.sched.AdmissionControl.Store(false)
		c.recordAction("CASE3", fmt.Sprintf(
			"p95=%.1fms < SLO*0.7=%.0fms, cpu=%.1f%% < 60%% → workers %d→%d, admission_control=OFF",
			p95, c.sloMs*0.7, cpuPct, currentWorkers, newWorkers,
		))
		c.mu.Lock()
		c.state.MaxWorkers = newWorkers
		c.state.AdmissionControl = false
		c.mu.Unlock()
		return
	}

	// Case 4: Memory pressure → reduce batch size
	if memPct > memThreshold {
		newBatch := max(1, currentBatch-1)
		c.pool.SetBatchSize(newBatch)
		c.recordAction("CASE4", fmt.Sprintf(
			"memory=%.1f%% > %.0f%% → batch %d→%d",
			memPct, memThreshold, currentBatch, newBatch,
		))
		c.mu.Lock()
		c.state.BatchSize = newBatch
		c.mu.Unlock()
	}
}

func (c *AdaptiveController) recordAction(caseLabel, reason string) {
	now := time.Now().UTC().Format(time.RFC3339)
	msg := fmt.Sprintf("[Controller] %s [%s] %s", now, caseLabel, reason)
	log.Println(msg)
	c.mu.Lock()
	c.state.LastAction = fmt.Sprintf("[%s] %s", caseLabel, reason)
	c.state.LastActionTime = now
	c.mu.Unlock()
}

// GetState returns a copy of the current controller state.
func (c *AdaptiveController) GetState() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.state
	s.AdmissionControl = c.sched.AdmissionControl.Load()
	w, b := c.pool.State()
	s.MaxWorkers = w
	s.BatchSize = b
	return s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
