# AETHER NOC - Network Monitoring Dashboard

AETHER NOC is a multi-tenant Network Monitoring Dashboard designed to provide organizations with real-time visibility and control over their network infrastructure.

The core purpose of this platform is to:
- **Discover Network Hosts**: Scan CIDR(IPv4 or IPv6) ranges via Ping and port-knocking to automatically detect and enroll new devices.
- **Isolate Tenant Data**: Enforce strict tenant isolation with organization-level partitioning across HTTP REST APIs and real-time WebSocket communication.
- **Prevent Alert Fatigue**: Reduce alarm noise by utilizing parent-dependency status checks to suppress cascade alerts on child devices when a parent router or switch goes offline.
- **Track Telemetry & Health**: Monitor device latency history, packet loss, and system performance, exposing health metrics via a Prometheus scraper endpoint.
