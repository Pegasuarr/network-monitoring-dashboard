package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/user/network-monitoring/internal/model"
	"github.com/user/network-monitoring/internal/repository"
	"github.com/user/network-monitoring/internal/service"
	"github.com/user/network-monitoring/internal/websocket"
)

type Scheduler struct {
	engine       *Engine
	alertService *service.AlertService
	hub          *websocket.Hub
	lastChecked  map[uuid.UUID]time.Time
	mu           sync.Mutex
}

func NewScheduler(engine *Engine, alertService *service.AlertService, hub *websocket.Hub) *Scheduler {
	return &Scheduler{
		engine:       engine,
		alertService: alertService,
		hub:          hub,
		lastChecked:  make(map[uuid.UUID]time.Time),
	}
}

func (s *Scheduler) Start(ctx context.Context) {
	slog.Info("Starting network monitoring background scheduler...")

	// Tick every 5 seconds to scan for due monitoring tasks
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Stopping background scheduler...")
			return
		case <-ticker.C:
			s.pollAndExecute(ctx)
		}
	}
}

func (s *Scheduler) pollAndExecute(ctx context.Context) {
	// Retrieve all active/enabled devices from database
	var devices []model.Device
	err := repository.DB.Where("enabled = ?", true).Find(&devices).Error
	if err != nil {
		slog.Error("Scheduler failed to fetch enabled devices", "error", err)
		return
	}

	now := time.Now()
	var wg sync.WaitGroup

	for _, device := range devices {
		s.mu.Lock()
		lastTime, exists := s.lastChecked[device.ID]
		s.mu.Unlock()

		interval := time.Duration(device.MonitoringInterval) * time.Second
		if interval <= 0 {
			interval = 60 * time.Second
		}

		// Run check if never checked or due
		if !exists || now.Sub(lastTime) >= interval {
			s.mu.Lock()
			s.lastChecked[device.ID] = now
			s.mu.Unlock()

			wg.Add(1)
			go func(d model.Device) {
				defer wg.Done()
				s.executeDeviceCheck(d)
			}(device)
		}
	}

	wg.Wait()
}

func (s *Scheduler) executeDeviceCheck(d model.Device) {
	// 1. Check if device is in maintenance mode
	now := time.Now()
	if d.MaintenanceStart != nil && d.MaintenanceEnd != nil {
		if now.After(*d.MaintenanceStart) && now.Before(*d.MaintenanceEnd) {
			// Device is in maintenance, skip executing checks and force status
			if d.Status != "maintenance" {
				repository.DB.Model(&model.Device{}).Where("id = ?", d.ID).Update("status", "maintenance")
				s.broadcastStatus(d.OrganizationID, d.ID, "maintenance")
			}
			return
		}
	}

	// 2. Run active monitoring probes
	result := s.engine.RunAllChecks(&d)

	// 3. Determine device status
	prevStatus := d.Status
	newStatus := "online"

	if result.PacketLossPct == 100 || !result.DNSResolved || (result.HTTPStatus >= 400 || (d.DeviceType == "server" && result.HTTPStatus == 0 && result.ResponseTimeMS == 0)) {
		newStatus = "offline"
	} else if result.PacketLossPct > 0 || result.LatencyMS > 100 {
		newStatus = "warning"
	}

	// 4. Save results to DB
	err := repository.DB.Create(result).Error
	if err != nil {
		slog.Error("Failed to save monitoring result to database", "device_id", d.ID, "error", err)
	}

	// 5. Check dependencies before setting status to offline
	if newStatus == "offline" && d.ParentID != nil {
		var parent model.Device
		err := repository.DB.Where("id = ?", *d.ParentID).First(&parent).Error
		if err == nil && parent.Status == "offline" {
			// Suppress alert, set status to unreachable
			newStatus = "unreachable"
		}
	}

	// Update device status in DB if changed
	if prevStatus != newStatus {
		repository.DB.Model(&model.Device{}).Where("id = ?", d.ID).Update("status", newStatus)
		d.Status = newStatus
	}

	// 6. Evaluate Alert Rules for this device check
	s.alertService.EvaluateRules(&d, result)

	// 7. WebSocket Live Broadcast
	s.broadcastResult(d.OrganizationID, d.ID, result)
	if prevStatus != newStatus {
		s.broadcastStatus(d.OrganizationID, d.ID, newStatus)
	}
}

func (s *Scheduler) broadcastResult(orgID uuid.UUID, deviceID uuid.UUID, res *model.MonitoringResult) {
	s.hub.Broadcast <- websocket.BroadcastEvent{
		OrgID: orgID,
		Type:  "ping_result",
		Payload: map[string]interface{}{
			"device_id":         deviceID,
			"latency_ms":        res.LatencyMS,
			"packet_loss_pct":   res.PacketLossPct,
			"response_time_ms":  res.ResponseTimeMS,
			"http_status":       res.HTTPStatus,
			"ssl_days":          res.SSLDaysRemaining,
			"dns_resolved":      res.DNSResolved,
			"cpu_usage":         res.CPUUsage,
			"ram_usage":         res.RAMUsage,
			"disk_usage":        res.DiskUsage,
			"checked_at":        res.CheckedAt,
		},
	}
}

func (s *Scheduler) broadcastStatus(orgID uuid.UUID, deviceID uuid.UUID, status string) {
	s.hub.Broadcast <- websocket.BroadcastEvent{
		OrgID: orgID,
		Type:  "device_status",
		Payload: map[string]interface{}{
			"device_id": deviceID,
			"status":    status,
		},
	}
}
