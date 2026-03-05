package api

import (
	"log"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/helios/internal/controller"
	"github.com/helios/internal/metrics"
	"github.com/helios/internal/scheduler"
	"github.com/helios/internal/worker"
)

// Handler holds all component references for the API.
type Handler struct {
	collector  *metrics.Collector
	sched      *scheduler.Scheduler
	pool       *worker.Pool
	ctrl       *controller.AdaptiveController
	inputDim   int
	requestTimeout time.Duration
}

// New creates the Handler.
func New(
	collector *metrics.Collector,
	sched *scheduler.Scheduler,
	pool *worker.Pool,
	ctrl *controller.AdaptiveController,
) *Handler {
	inputDim := 128
	if v := os.Getenv("HELIOS_MODEL_INPUT_DIM"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			inputDim = i
		}
	}
	return &Handler{
		collector:      collector,
		sched:          sched,
		pool:           pool,
		ctrl:           ctrl,
		inputDim:       inputDim,
		requestTimeout: 10 * time.Second,
	}
}

// RegisterRoutes attaches all routes to the engine.
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.POST("/predict", h.Predict)
	r.GET("/metrics", h.Metrics)
	r.GET("/metrics/history", h.MetricsHistory)
	r.GET("/status", h.Status)
	r.GET("/health", h.Health)
	r.POST("/simulate", h.Simulate)
}

// PredictRequest is the JSON body for POST /predict.
type PredictRequest struct {
	Input    [][]float64 `json:"input"    binding:"required"`
	Priority string      `json:"priority" binding:"required"`
}

// PredictResponse is the success response for POST /predict.
type PredictResponse struct {
	RequestID    string    `json:"request_id"`
	Output       []float64 `json:"output"`
	QueueWaitMs  float64   `json:"queue_wait_ms"`
	InferenceMs  float64   `json:"inference_ms"`
}

// Predict handles POST /predict.
// No inference runs in this goroutine. It enqueues and waits.
func (h *Handler) Predict(c *gin.Context) {
	var body PredictRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate priority
	if err := scheduler.ValidatePriority(body.Priority); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate input dimensions
	for i, row := range body.Input {
		if len(row) != h.inputDim {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error": "input dimension mismatch",
				"detail": map[string]interface{}{
					"row":      i,
					"expected": h.inputDim,
					"got":      len(row),
				},
			})
			return
		}
	}

	req := &scheduler.Request{
		ID:       uuid.NewString(),
		Priority: body.Priority,
		Input:    body.Input,
	}

	enqueued, reason := h.sched.Enqueue(req)
	if !enqueued {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":  "overloaded",
			"reason": reason,
		})
		return
	}

	// Wait for inference to complete (or timeout)
	select {
	case <-req.Done:
	case <-time.After(h.requestTimeout):
		c.JSON(http.StatusGatewayTimeout, gin.H{
			"error":  "timeout",
			"reason": "inference did not complete within 10s",
		})
		return
	}

	if req.Err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":  "inference_failed",
			"reason": req.Err.Error(),
		})
		return
	}

	queueWaitMs := float64(time.Since(req.EnqueueTime).Microseconds()) / 1000.0

	c.JSON(http.StatusOK, PredictResponse{
		RequestID:   req.ID,
		Output:      req.Output,
		QueueWaitMs: queueWaitMs - req.InferenceMs,
		InferenceMs: req.InferenceMs,
	})
}

// Metrics handles GET /metrics.
// Returns null for latency fields when insufficient data — never fakes zeros.
func (h *Handler) Metrics(c *gin.Context) {
	snap := h.collector.GetSnapshot()
	if snap == nil {
		// No tick has occurred yet (server just started)
		c.JSON(http.StatusOK, gin.H{
			"p50":            nil,
			"p95":            nil,
			"p99":            nil,
			"cpu":            nil,
			"memory":         nil,
			"queue_depth":    0,
			"active_workers": 0,
			"max_workers":    0,
			"batch_size":     0,
			"throughput":     0,
			"note":           "no_data_yet",
		})
		return
	}
	c.JSON(http.StatusOK, snap)
}

// MetricsHistory handles GET /metrics/history.
func (h *Handler) MetricsHistory(c *gin.Context) {
	history := h.collector.GetHistory()
	c.JSON(http.StatusOK, history)
}

// Status handles GET /status — returns controller state.
func (h *Handler) Status(c *gin.Context) {
	state := h.ctrl.GetState()
	c.JSON(http.StatusOK, state)
}

// Health handles GET /health.
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// SimulateRequest is the body for POST /simulate.
type SimulateRequest struct {
	Scenario        string `json:"scenario"         binding:"required"`
	DurationSeconds int    `json:"duration_seconds" binding:"required"`
}

// Simulate handles POST /simulate — injects failure scenarios.
func (h *Handler) Simulate(c *gin.Context) {
	var body SimulateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	duration := time.Duration(body.DurationSeconds) * time.Second

	switch body.Scenario {
	case "delay":
		// Inject sleep into inference execution for N seconds
		h.pool.SimulateDelay.Store(true)
		go func() {
			time.Sleep(duration)
			h.pool.SimulateDelay.Store(false)
			log.Printf("[Simulate] delay injection ended after %ds", body.DurationSeconds)
		}()

	case "cpu_stress":
		// Spin goroutines on all cores for N seconds
		numCores := runtime.NumCPU()
		stop := make(chan struct{})
		for i := 0; i < numCores; i++ {
			go func() {
				for {
					select {
					case <-stop:
						return
					default:
						// Pure CPU burn
						_ = 0
					}
				}
			}()
		}
		go func() {
			time.Sleep(duration)
			close(stop)
			log.Printf("[Simulate] cpu_stress ended after %ds", body.DurationSeconds)
		}()

	case "memory_spike":
		// Allocate and hold ~500MB for N seconds
		go func() {
			spike := make([]byte, 500*1024*1024)
			// Touch pages to force actual allocation
			for i := range spike {
				spike[i] = byte(rand.Intn(256))
			}
			log.Printf("[Simulate] memory_spike allocated %dMB", len(spike)/(1024*1024))
			time.Sleep(duration)
			spike = nil
			log.Printf("[Simulate] memory_spike released after %ds", body.DurationSeconds)
		}()

	case "worker_crash":
		// Next 3 worker executions will panic and recover
		h.pool.SimulateCrash.Store(3)
		log.Printf("[Simulate] worker_crash: next 3 inferences will fail")

	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "unknown scenario",
			"valid":   []string{"delay", "cpu_stress", "memory_spike", "worker_crash"},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"scenario":         body.Scenario,
		"duration_seconds": body.DurationSeconds,
		"started":          true,
	})
}

// SimulateCrash field on Pool needs to be atomic.Int32 — wire it here.
// (The pool exposes it directly via atomic so no wrapper needed.)
var _ = atomic.Int32{}
