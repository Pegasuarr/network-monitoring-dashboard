package service

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/user/network-monitoring/internal/model"
)

type DiscoveryService struct{}

func NewDiscoveryService() *DiscoveryService {
	return &DiscoveryService{}
}

type DiscoveredDevice struct {
	IPAddress  string   `json:"ip_address"`
	Hostname   string   `json:"hostname"`
	Vendor     string   `json:"vendor"`
	OS         string   `json:"os"`
	DeviceType string   `json:"device_type"`
	OpenPorts  []int    `json:"open_ports"`
	PingTimeMS float64  `json:"ping_time_ms"`
}

func (s *DiscoveryService) ScanCIDR(ctx context.Context, cidr string) ([]DiscoveredDevice, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR format: %v", err)
	}

	var ips []string
	for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
		ips = append(ips, ip.String())
	}

	// Remove network and broadcast address if it's a standard /24 or smaller
	if len(ips) > 2 {
		ips = ips[1 : len(ips)-1]
	}

	var wg sync.WaitGroup
	ipChan := make(chan string, len(ips))
	resultsChan := make(chan DiscoveredDevice, len(ips))

	// Worker pool size
	workerCount := 50
	if len(ips) < workerCount {
		workerCount = len(ips)
	}

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for targetIP := range ipChan {
				select {
				case <-ctx.Done():
					return
				default:
					device, alive := s.scanIP(targetIP)
					if alive {
						resultsChan <- device
					}
				}
			}
		}()
	}

	for _, targetIP := range ips {
		ipChan <- targetIP
	}
	close(ipChan)

	wg.Wait()
	close(resultsChan)

	var discovered []DiscoveredDevice
	for res := range resultsChan {
		discovered = append(discovered, res)
	}

	return discovered, nil
}

func (s *DiscoveryService) scanIP(ip string) (DiscoveredDevice, bool) {
	// 1. Check common TCP ports to see if host is alive
	commonPorts := []int{22, 80, 443, 135, 445, 3389, 8080}
	var openPorts []int
	var alive bool
	var rtt float64

	for _, port := range commonPorts {
		address := fmt.Sprintf("%s:%d", ip, port)
		start := time.Now()
		conn, err := net.DialTimeout("tcp", address, 300*time.Millisecond)
		if err == nil {
			conn.Close()
			openPorts = append(openPorts, port)
			alive = true
			if rtt == 0 {
				rtt = float64(time.Since(start).Milliseconds())
			}
		}
	}

	// Fallback to ping equivalent lookup
	if !alive {
		// Try TCP connection to port 53 (DNS) or 80 just in case
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:53", ip), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			alive = true
			rtt = 10.0
		}
	}

	if !alive {
		return DiscoveredDevice{}, false
	}

	// 2. Resolve Hostname
	hostname := ""
	addrs, err := net.LookupAddr(ip)
	if err == nil && len(addrs) > 0 {
		hostname = strings.TrimSuffix(addrs[0], ".")
	} else {
		hostname = fmt.Sprintf("host-%s", strings.ReplaceAll(ip, ".", "-"))
	}

	// 3. Fingerprint Device Type & OS
	deviceType := "workstation"
	os := "Unknown OS"
	vendor := "Unknown Vendor"

	hasSSH := false
	hasHTTP := false
	hasHTTPS := false
	hasRDP := false
	hasSMB := false

	for _, p := range openPorts {
		switch p {
		case 22:
			hasSSH = true
		case 80:
			hasHTTP = true
		case 443:
			hasHTTPS = true
		case 3389:
			hasRDP = true
		case 135, 445:
			hasSMB = true
		}
	}

	if hasSSH {
		deviceType = "server"
		os = "Linux/Unix"
		vendor = "Generic Linux"
	}
	if hasSMB || hasRDP {
		deviceType = "workstation"
		os = "Windows"
		vendor = "Microsoft"
	}
	if hasHTTP || hasHTTPS {
		// If port 80/443 but no SSH/RDP, might be web server or networking gear
		if !hasSSH && !hasRDP {
			deviceType = "switch"
			os = "Embedded OS"
			vendor = "Cisco/HP"
		} else {
			deviceType = "server"
			os = "Linux Server"
		}
	}

	return DiscoveredDevice{
		IPAddress:  ip,
		Hostname:   hostname,
		Vendor:     vendor,
		OS:         os,
		DeviceType: deviceType,
		OpenPorts:  openPorts,
		PingTimeMS: rtt,
	}, true
}

func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// Auto-add device conversion
func (s *DiscoveryService) AutoAdd(orgID uuid.UUID, dev DiscoveredDevice) *model.Device {
	return &model.Device{
		ID:                 uuid.New(),
		OrganizationID:     orgID,
		Name:               dev.Hostname,
		Hostname:           dev.IPAddress,
		IPAddress:          dev.IPAddress,
		DeviceType:         dev.DeviceType,
		OS:                 dev.OS,
		Vendor:             dev.Vendor,
		Status:             "online",
		MonitoringInterval: 60,
		Enabled:            true,
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
}
