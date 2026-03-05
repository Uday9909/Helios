package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/helios/internal/api"
	"github.com/helios/internal/controller"
	"github.com/helios/internal/metrics"
	"github.com/helios/internal/model"
	"github.com/helios/internal/scheduler"
	"github.com/helios/internal/worker"
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("[Helios] Starting up...")

	// ── 1. Model ─────────────────────────────────────────────────────────────
	inputDim := envInt("HELIOS_MODEL_INPUT_DIM", 128)
	mdl, err := model.Load(inputDim)
	if err != nil {
		log.Fatalf("[Helios] FATAL: failed to load model: %v", err)
	}

	// ── 2. Metrics collector ─────────────────────────────────────────────────
	collector := metrics.New()
	// References injected after all components are built (step 6)

	// ── 3. Scheduler ─────────────────────────────────────────────────────────
	maxQueueSize := envInt("HELIOS_MAX_QUEUE_SIZE", 500)
	sched := scheduler.New(maxQueueSize)

	// ── 4. Worker pool ───────────────────────────────────────────────────────
	maxWorkers := envInt("HELIOS_MAX_WORKERS", runtime.NumCPU())
	batchSize := envInt("HELIOS_BATCH_SIZE", 8)
	pool := worker.New(sched, collector, mdl, maxWorkers, batchSize)

	// ── 5. Controller ────────────────────────────────────────────────────────
	ctrl := controller.New(collector, pool, sched)

	// ── 6. Inject references into collector ──────────────────────────────────
	collector.SetReferences(
		func() int { return sched.QueueDepth() },
		func() int { return pool.ActiveCount() },
		func() (int, int) { return pool.State() },
	)

	// ── 7. Start all components ───────────────────────────────────────────────
	collector.Start()
	pool.Start()
	ctrl.Start()
	log.Println("[Helios] All components started")

	// ── 8. HTTP server ────────────────────────────────────────────────────────
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	// CORS for dashboard at localhost:5173
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"http://localhost:5173", "http://127.0.0.1:5173"},
		AllowMethods:     []string{"GET", "POST", "OPTIONS"},
		AllowHeaders:     []string{"Content-Type", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: false,
		MaxAge:           12 * time.Hour,
	}))

	handlers := api.New(collector, sched, pool, ctrl)
	handlers.RegisterRoutes(r)

	port := os.Getenv("HELIOS_PORT")
	if port == "" {
		port = "8000"
	}

	srv := &http.Server{
		Addr:    "0.0.0.0:" + port,
		Handler: r,
	}

	// ── 9. Graceful shutdown ──────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("[Helios] Listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Helios] Server error: %v", err)
		}
	}()

	<-quit
	log.Println("[Helios] Shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[Helios] HTTP shutdown error: %v", err)
	}

	ctrl.Stop()
	pool.Stop()
	collector.Stop()

	log.Println("[Helios] Shutdown complete")
}
