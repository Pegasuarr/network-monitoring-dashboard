package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/user/network-monitoring/configs"
	"github.com/user/network-monitoring/internal/api"
	"github.com/user/network-monitoring/internal/monitor"
	"github.com/user/network-monitoring/internal/repository"
	"github.com/user/network-monitoring/internal/service"
	"github.com/user/network-monitoring/internal/websocket"
	"github.com/user/network-monitoring/pkg/logger"
)

func main() {
	// 1. Initialize Logger
	logger.InitLogger()
	slog.Info("Starting Enterprise Network Monitoring Server...")

	// 2. Load Configuration
	cfg := configs.LoadConfig()

	// 3. Connect to Database (Postgres)
	db, err := repository.InitDB(cfg)
	if err != nil {
		slog.Error("Database initialization failed", "error", err)
		os.Exit(1)
	}
	slog.Info("PostgreSQL connection established and tables migrated.")

	// 4. Seeding Initial Data
	if err := repository.Seed(db); err != nil {
		slog.Error("Database seeding failed", "error", err)
		os.Exit(1)
	}
	slog.Info("Database seeding verified successfully.")

	// 5. Connect to Cache (Redis)
	_, err = repository.InitRedis(cfg)
	if err != nil {
		slog.Warn("Redis cache connection failed. Rate limits will run in-memory bypass.", "error", err)
	} else {
		slog.Info("Redis session cache connected successfully.")
	}

	// 6. Initialize WebSocket Hub
	wsHub := websocket.NewHub()
	go wsHub.Run()
	slog.Info("WebSocket Hub running.")

	// 7. Setup Clean Architecture Service Dependencies
	userRepo := repository.NewUserRepository(db)
	deviceRepo := repository.NewDeviceRepository(db)
	alertRepo := repository.NewAlertRepository(db)

	authService := service.NewAuthService(userRepo, cfg)
	deviceService := service.NewDeviceService(deviceRepo)
	alertService := service.NewAlertService(alertRepo)
	discoveryService := service.NewDiscoveryService()
	dashboardService := service.NewDashboardService(deviceRepo, alertRepo)

	// Control flag for pausing scheduler execution
	isMonPaused := false

	// 8. Start Background Task Scheduler
	monitorEngine := monitor.NewEngine()
	scheduler := monitor.NewScheduler(monitorEngine, alertService, wsHub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		// Periodically verify if monitoring scheduler is running or paused
		schedulerTicker := time.NewTicker(2 * time.Second)
		defer schedulerTicker.Stop()

		schedulerRunningCtx, schedulerCancel := context.WithCancel(ctx)
		go scheduler.Start(schedulerRunningCtx)

		for {
			select {
			case <-ctx.Done():
				schedulerCancel()
				return
			case <-schedulerTicker.C:
				if isMonPaused && schedulerRunningCtx != nil {
					schedulerCancel()
					schedulerRunningCtx = nil
					slog.Info("Background monitoring tasks have been PAUSED.")
				} else if !isMonPaused && schedulerRunningCtx == nil {
					schedulerRunningCtx, schedulerCancel = context.WithCancel(ctx)
					go scheduler.Start(schedulerRunningCtx)
					slog.Info("Background monitoring tasks have been RESUMED.")
				}
			}
		}
	}()

	// 9. Configure REST Router
	handlers := api.NewHandlers(
		authService,
		deviceService,
		alertService,
		discoveryService,
		dashboardService,
		userRepo,
		&isMonPaused,
	)
	router := api.SetupRouter(handlers, wsHub)

	// 10. Start HTTP Server
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: router,
	}

	go func() {
		slog.Info("Server listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP Server listening failed", "error", err)
			os.Exit(1)
		}
	}()

	// 11. Graceful Shutdown listener
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("Shutting down server gracefully...")

	cancel() // Cancel context to stop background monitor routines

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP Server forced to shutdown", "error", err)
	}

	slog.Info("Server exited.")
}
