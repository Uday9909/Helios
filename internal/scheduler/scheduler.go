package scheduler

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Request represents an inference request in the system.
type Request struct {
	ID           string
	Priority     string // "premium" or "standard"
	Input        [][]float64
	EnqueueTime  time.Time

	// Set by worker after inference completes
	Output       []float64
	InferenceMs  float64
	Err          error
	Done         chan struct{}
}

// Scheduler routes requests into priority queues with weighted fair scheduling.
type Scheduler struct {
	premium      chan *Request
	standard     chan *Request
	maxQueueSize int

	AdmissionControl atomic.Bool // set by controller; true = reject standard

	mu        sync.Mutex
	wfsCount  int // weighted fair scheduling counter
}

// New creates a Scheduler with the given total max queue size.
func New(maxQueueSize int) *Scheduler {
	// Split capacity proportionally: premium gets 40%, standard 60%
	premiumCap := maxQueueSize * 2 / 5
	standardCap := maxQueueSize - premiumCap
	return &Scheduler{
		premium:      make(chan *Request, premiumCap),
		standard:     make(chan *Request, standardCap),
		maxQueueSize: maxQueueSize,
	}
}

// Enqueue adds a request to the appropriate queue.
// Returns an error string if the request is rejected.
func (s *Scheduler) Enqueue(req *Request) (bool, string) {
	// Check total queue depth
	if s.QueueDepth() >= s.maxQueueSize {
		return false, "queue_full"
	}

	// Admission control: reject standard under pressure
	if req.Priority == "standard" && s.AdmissionControl.Load() {
		return false, "admission_control"
	}

	req.EnqueueTime = time.Now()
	req.Done = make(chan struct{})

	if req.Priority == "premium" {
		select {
		case s.premium <- req:
			return true, ""
		default:
			return false, "queue_full"
		}
	} else {
		select {
		case s.standard <- req:
			return true, ""
		default:
			return false, "queue_full"
		}
	}
}

// Dequeue returns the next request using weighted fair scheduling.
// Returns nil if both queues are empty.
// WFS policy: 3 premium, 1 standard, cycling. Never starves standard.
func (s *Scheduler) Dequeue() *Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	// WFS: serve 3 premium then 1 standard
	if s.wfsCount < 3 {
		// Try premium first
		select {
		case req := <-s.premium:
			s.wfsCount++
			return req
		default:
		}
		// Premium empty, fall through to standard
	}

	// Try standard
	select {
	case req := <-s.standard:
		s.wfsCount = 0
		return req
	default:
	}

	// Standard empty too, try premium regardless of counter
	select {
	case req := <-s.premium:
		s.wfsCount++
		return req
	default:
	}

	return nil
}

// QueueDepth returns the real-time total count across both queues.
func (s *Scheduler) QueueDepth() int {
	return len(s.premium) + len(s.standard)
}

// Stats returns individual queue depths for diagnostics.
func (s *Scheduler) Stats() (premium, standard int) {
	return len(s.premium), len(s.standard)
}

// ValidatePriority checks that priority is a known value.
func ValidatePriority(p string) error {
	if p != "premium" && p != "standard" {
		return fmt.Errorf("priority must be 'premium' or 'standard', got %q", p)
	}
	return nil
}
