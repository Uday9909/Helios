package worker

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helios/internal/metrics"
	"github.com/helios/internal/model"
	"github.com/helios/internal/scheduler"
)

// Pool manages a pool of goroutines that pull from the scheduler and run inference.
type Pool struct {
	sched      *scheduler.Scheduler
	collector  *metrics.Collector
	model      *model.Model

	mu         sync.Mutex
	maxWorkers int
	batchSize  int

	activeCount atomic.Int64

	// Semaphore channel controls concurrency: buffered chan of size maxWorkers
	sem     chan struct{}

	stop    chan struct{}
	stopped chan struct{}

	// Failure simulation
	SimulateDelay   atomic.Bool
	SimulateCrash   atomic.Int32 // countdown of crashes remaining
}

// New creates a Pool. Call Start() to begin dispatching.
func New(sched *scheduler.Scheduler, collector *metrics.Collector, mdl *model.Model, maxWorkers, batchSize int) *Pool {
	return &Pool{
		sched:      sched,
		collector:  collector,
		model:      mdl,
		maxWorkers: maxWorkers,
		batchSize:  batchSize,
		sem:        make(chan struct{}, maxWorkers),
		stop:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}
}

// Start launches the dispatch loop.
func (p *Pool) Start() {
	go p.dispatchLoop()
}

// Stop drains in-flight work and shuts down.
func (p *Pool) Stop() {
	close(p.stop)
	<-p.stopped
}

func (p *Pool) dispatchLoop() {
	defer close(p.stopped)
	for {
		select {
		case <-p.stop:
			return
		default:
		}

		req := p.sched.Dequeue()
		if req == nil {
			time.Sleep(2 * time.Millisecond)
			continue
		}

		// Acquire semaphore slot (blocks if at max concurrency)
		select {
		case p.sem <- struct{}{}:
		case <-p.stop:
			// Reject pending request on shutdown
			req.Err = fmt.Errorf("server shutting down")
			close(req.Done)
			return
		}

		go p.execute(req)
	}
}

func (p *Pool) execute(req *scheduler.Request) {
	defer func() {
		// Always release semaphore
		<-p.sem
	}()

	// Simulated crash mode
	if p.SimulateCrash.Load() > 0 {
		p.SimulateCrash.Add(-1)
		req.Err = fmt.Errorf("simulated worker crash")
		log.Printf("[Worker] Simulated crash for request %s", req.ID)
		close(req.Done)
		return
	}

	p.activeCount.Add(1)
	defer p.activeCount.Add(-1)

	// Simulated delay injection
	if p.SimulateDelay.Load() {
		time.Sleep(300 * time.Millisecond)
	}

	// Run real inference
	output, latencyMs, err := p.model.RunInference(req.Input)
	if err != nil {
		req.Err = fmt.Errorf("inference failed: %w", err)
		log.Printf("[Worker] Inference error for %s: %v", req.ID, err)
		close(req.Done)
		return
	}

	req.Output = output
	req.InferenceMs = latencyMs

	// Record real latency
	p.collector.RecordLatency(latencyMs)

	close(req.Done)
}

// SetMaxWorkers dynamically adjusts the concurrency limit.
// This rebuilds the semaphore — in-flight goroutines finish, new ones are gated by new limit.
func (p *Pool) SetMaxWorkers(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if n == p.maxWorkers {
		return
	}

	old := p.maxWorkers
	p.maxWorkers = n

	// Drain old semaphore and create new one with updated capacity
	// Wait for all current slots to be released (in-flight to finish)
	oldSem := p.sem
	newSem := make(chan struct{}, n)

	// Drain old sem by waiting for it to empty (all goroutines release)
	// We swap the sem pointer; existing goroutines release to the old sem,
	// new dispatches will use new sem
	p.sem = newSem

	// Drain old sem in background (blocks until all in-flight finish)
	go func() {
		for i := 0; i < cap(oldSem); i++ {
			oldSem <- struct{}{}
		}
		log.Printf("[WorkerPool] Drained old semaphore (cap=%d)", cap(oldSem))
	}()

	log.Printf("[WorkerPool] max_workers changed %d → %d", old, n)
}

// SetBatchSize updates the batch size (used by controller, exposed to status).
func (p *Pool) SetBatchSize(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.batchSize = n
	log.Printf("[WorkerPool] batch_size changed to %d", n)
}

// ActiveCount returns the number of goroutines currently running inference.
func (p *Pool) ActiveCount() int {
	return int(p.activeCount.Load())
}

// State returns current max_workers and batch_size.
func (p *Pool) State() (maxWorkers, batchSize int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxWorkers, p.batchSize
}
