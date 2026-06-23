package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	socksPort      int
	httpPort       int
	testPort       int
	bindAddr       string
	verbose        bool
	upstreamEnable bool
	upstreamIP     string
	upstreamPort   int
	upstreamType   string

	connections   int64
	activeConns   int64
	totalUpload   int64
	totalDownload int64

	// Per-device tracking
	deviceMutex sync.RWMutex
	devices     = make(map[string]*DeviceStats)

	// Connection logging
	logMutex    sync.Mutex
	recentConns = make([]ConnectionLog, 0, 100)

	// Buffer pool for better memory usage
	bufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 32*1024)
			return &buf
		},
	}
)

type DeviceStats struct {
	MAC        string
	IP         string
	Upload     int64
	Download   int64
	LastSeen   time.Time
	ConnCount  int64
	LastTarget string
}

type ConnectionLog struct {
	Time       time.Time
	ConnID     int64
	ClientIP   string
	ClientMAC  string
	Target     string
	Protocol   string
	Status     string
	Upload     int64
	Download   int64
}

func main() {
	flag.IntVar(&socksPort, "socks", 18000, "SOCKS5 port to listen on")
	flag.IntVar(&httpPort, "http", 18009, "HTTP proxy port to listen on")
	flag.IntVar(&testPort, "test", 10810, "Test HTTP server port")
	flag.StringVar(&bindAddr, "bind", "0.0.0.0", "Bind address (IP to listen on)")
	flag.BoolVar(&verbose, "v", true, "Verbose logging")
	flag.BoolVar(&upstreamEnable, "upstream", false, "Enable upstream proxy")
	flag.StringVar(&upstreamIP, "upstream-ip", "127.0.0.1", "Upstream proxy IP address")
	flag.IntVar(&upstreamPort, "upstream-port", 18000, "Upstream proxy port")
	flag.StringVar(&upstreamType, "upstream-type", "socks5", "Upstream proxy type (socks5/http)")
	flag.Parse()

	clearScreen()
	printBanner()

	ips := getLocalIPs()
	primaryIP := "127.0.0.1"
	if len(ips) > 0 {
		primaryIP = ips[0]
	}

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║          VPN SHARE FOR NEKOBOX / V2RAYNG / TOR              ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	fmt.Println("📡 Your IP addresses:")
	for _, ip := range ips {
		fmt.Printf("   → %s\n", ip)
	}
	fmt.Println()

	// Check upstream proxy if enabled
	if upstreamEnable {
		upstreamAddr := fmt.Sprintf("%s:%d", upstreamIP, upstreamPort)
		fmt.Printf("🔗 Upstream Proxy ENABLED\n")
		fmt.Printf("   Type: %s\n", strings.ToUpper(upstreamType))
		fmt.Printf("   Address: %s\n", upstreamAddr)

		if testUpstreamConnection(upstreamIP, upstreamPort) {
			fmt.Println("   ✓ Upstream proxy connection OK")
		} else {
			fmt.Println("   ✗ WARNING: Cannot connect to upstream proxy!")
			fmt.Printf("   Make sure %s proxy is running on %s\n", strings.ToUpper(upstreamType), upstreamAddr)
		}
		fmt.Println()
	} else {
		fmt.Println("📡 Direct connection mode (no upstream proxy)")
		fmt.Println()
	}

	// Disable firewall on Windows
	if runtime.GOOS == "windows" {
		exec.Command("netsh", "advfirewall", "set", "allprofiles", "state", "off").Run()
		fmt.Println("✓ Windows Firewall disabled")
		fmt.Println()
	}

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start servers with context
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		startTestServer(ctx, primaryIP)
	}()

	go func() {
		defer wg.Done()
		startSOCKS5Server(ctx)
	}()

	go func() {
		defer wg.Done()
		startHTTPProxy(ctx)
	}()

	// Start stats display goroutine
	go displayStats(ctx)

	// Start device cleanup goroutine
	go cleanupInactiveDevices(ctx)

	time.Sleep(500 * time.Millisecond)

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    ✅ SERVERS RUNNING                        ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  SOCKS5:     %s:%-39d  ║\n", primaryIP, socksPort)
	fmt.Printf("║  HTTP:       %s:%-39d  ║\n", primaryIP, httpPort)
	fmt.Printf("║  Test Page:  http://%s:%-30d  ║\n", primaryIP, testPort)
	if upstreamEnable {
		fmt.Println("╠══════════════════════════════════════════════════════════════╣")
		fmt.Printf("║  🔗 Upstream: %s:%-33d  ║\n", upstreamIP, upstreamPort)
		fmt.Printf("║     Type: %-50s  ║\n", strings.ToUpper(upstreamType))
	}
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Println("║  📱 NekoBox Configuration:                                   ║")
	fmt.Println("║     + → Manual Settings → SOCKS                              ║")
	fmt.Printf("║     Server: %-48s  ║\n", primaryIP)
	fmt.Printf("║     Port: %-50d  ║\n", socksPort)
	fmt.Println("║     Username/Password: (leave EMPTY)                         ║")
	fmt.Println("║     TLS: OFF                                                 ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Println("║  📱 V2rayNG Configuration:                                   ║")
	fmt.Println("║     Type: Socks                                              ║")
	fmt.Printf("║     Address: %-47s  ║\n", primaryIP)
	fmt.Printf("║     Port: %-50d  ║\n", socksPort)
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("⏳ Waiting for connections... (Press Ctrl+C to stop)")
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println("                     🔴 LIVE CONNECTIONS                       ")
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println()

	// Wait for exit signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\n\n🛑 Shutting down gracefully...")
	cancel()

	// Print final stats
	printFinalStats()

	// Wait for servers to stop (with timeout)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		fmt.Println("✓ All servers stopped successfully.")
	case <-time.After(5 * time.Second):
		fmt.Println("⚠ Timeout waiting for servers, forcing exit.")
	}
}

func testUpstreamConnection(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func trackDevice(ip string, upload, download int64) {
	mac := getMACAddress(ip)

	deviceMutex.Lock()
	defer deviceMutex.Unlock()

	device, exists := devices[ip]
	if !exists {
		device = &DeviceStats{
			MAC: mac,
			IP:  ip,
		}
		devices[ip] = device
		log.Printf("📱 [NEW DEVICE] IP: %s | MAC: %s", ip, mac)
	}

	atomic.AddInt64(&device.Upload, upload)
	atomic.AddInt64(&device.Download, download)
	atomic.AddInt64(&device.ConnCount, 1)
	device.LastSeen = time.Now()
}

func logConnection(connLog ConnectionLog) {
	logMutex.Lock()
	defer logMutex.Unlock()

	// Keep only last 100 connections
	if len(recentConns) >= 100 {
		recentConns = recentConns[1:]
	}
	recentConns = append(recentConns, connLog)

	// Print live connection log
	status := "✓"
	if connLog.Status != "SUCCESS" {
		status = "✗"
	}

	fmt.Printf("[%s] %s [%s] %s (%s) → %s | ↑%s ↓%s\n",
		connLog.Time.Format("15:04:05"),
		status,
		connLog.Protocol,
		connLog.ClientIP,
		connLog.ClientMAC,
		connLog.Target,
		formatBytes(connLog.Upload),
		formatBytes(connLog.Download))
}

func getMACAddress(ip string) string {
	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		cmd = exec.Command("arp", "-a", ip)
	} else {
		cmd = exec.Command("arp", "-n", ip)
	}

	output, err := cmd.Output()
	if err != nil {
		return "Unknown"
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, ip) {
			fields := strings.Fields(line)
			for _, field := range fields {
				if strings.Count(field, ":") == 5 || strings.Count(field, "-") == 5 {
					return strings.ToUpper(strings.ReplaceAll(field, "-", ":"))
				}
			}
		}
	}

	return "Unknown"
}

func cleanupInactiveDevices(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deviceMutex.Lock()
			for ip, device := range devices {
				if time.Since(device.LastSeen) > 5*time.Minute {
					log.Printf("🔌 [DEVICE TIMEOUT] IP: %s | MAC: %s", ip, device.MAC)
					delete(devices, ip)
				}
			}
			deviceMutex.Unlock()
		}
	}
}

func displayStats(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			printDeviceStatsInline()
		}
	}
}

func printDeviceStatsInline() {
	deviceMutex.RLock()
	defer deviceMutex.RUnlock()

	if len(devices) == 0 {
		return
	}

	up := atomic.LoadInt64(&totalUpload)
	down := atomic.LoadInt64(&totalDownload)
	active := atomic.LoadInt64(&activeConns)
	total := atomic.LoadInt64(&connections)

	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Printf("📊 STATS | Active: %d | Total: %d | ↑%s ↓%s (Total: %s)\n",
		active, total, formatBytes(up), formatBytes(down), formatBytes(up+down))
	fmt.Println("───────────────────────────────────────────────────────────────")

	for _, device := range devices {
		fmt.Printf("📱 %s (%s) | Conns: %d | ↑%s ↓%s | Last: %s\n",
			device.IP,
			device.MAC,
			atomic.LoadInt64(&device.ConnCount),
			formatBytes(atomic.LoadInt64(&device.Upload)),
			formatBytes(atomic.LoadInt64(&device.Download)),
			device.LastTarget)
	}
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println()
}

func printFinalStats() {
	up := atomic.LoadInt64(&totalUpload)
	down := atomic.LoadInt64(&totalDownload)
	total := atomic.LoadInt64(&connections)

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                      📊 FINAL STATISTICS                     ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Total Connections:  %-40d ║\n", total)
	fmt.Printf("║  Total Upload:       %-40s ║\n", formatBytes(up))
	fmt.Printf("║  Total Download:     %-40s ║\n", formatBytes(down))
	fmt.Printf("║  Total Transfer:     %-40s ║\n", formatBytes(up+down))
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Println("║                    📱 DEVICE SUMMARY                         ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")

	deviceMutex.RLock()
	if len(devices) == 0 {
		fmt.Println("║  No devices connected                                        ║")
	} else {
		for _, device := range devices {
			fmt.Printf("║  %s (%s)\n", device.IP, device.MAC)
			fmt.Printf("║    Connections: %-44d ║\n", atomic.LoadInt64(&device.ConnCount))
			fmt.Printf("║    Upload:      %-44s ║\n", formatBytes(atomic.LoadInt64(&device.Upload)))
			fmt.Printf("║    Download:    %-44s ║\n", formatBytes(atomic.LoadInt64(&device.Download)))
			fmt.Println("╠══════════════════════════════════════════════════════════════╣")
		}
	}
	deviceMutex.RUnlock()

	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
}

func formatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func clearScreen() {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("cmd", "/c", "cls")
		cmd.Stdout = os.Stdout
		cmd.Run()
	} else {
		fmt.Print("\033[H\033[2J")
	}
}

func printBanner() {
	fmt.Println()
	fmt.Println("  ██╗   ██╗██████╗ ███╗   ██╗    ███████╗██╗  ██╗ █████╗ ██████╗ ███████╗")
	fmt.Println("  ██║   ██║██╔══██╗████╗  ██║    ██╔════╝██║  ██║██╔══██╗██╔══██╗██╔════╝")
	fmt.Println("  ██║   ██║██████╔╝██╔██╗ ██║    ███████╗███████║███████║██████╔╝█████╗  ")
	fmt.Println("  ╚██╗ ██╔╝██╔═══╝ ██║╚██╗██║    ╚════██║██╔══██║██╔══██║██╔══██╗██╔══╝  ")
	fmt.Println("   ╚████╔╝ ██║     ██║ ╚████║    ███████║██║  ██║██║  ██║██║  ██║███████╗")
	fmt.Println("    ╚═══╝  ╚═╝     ╚═╝  ╚═══╝    ╚══════╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚══════╝")
	fmt.Println()
	fmt.Println("                    Advanced Proxy Sharing Tool v2.0")
	fmt.Println()
}

func getLocalIPs() []string {
	var ips []string
	interfaces, _ := net.Interfaces()

	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					if !ip4.IsLinkLocalUnicast() && !ip4.IsLoopback() {
						ips = append(ips, ip4.String())
					}
				}
			}
		}
	}
	return ips
}

// ═══════════════════════════════════════════════════════════════════════════════
// TEST HTTP SERVER
// ═══════════════════════════════════════════════════════════════════════════════

func startTestServer(ctx context.Context, primaryIP string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		clientIP := strings.Split(r.RemoteAddr, ":")[0]
		log.Printf("[TEST] ✓ Browser connection from %s", clientIP)

		up := atomic.LoadInt64(&totalUpload)
		down := atomic.LoadInt64(&totalDownload)

		upstreamStatus := ""
		if upstreamEnable {
			upstreamStatus = fmt.Sprintf(`<div class="success" style="background: #2196F3;">
				🔗 Upstream Proxy Active: %s:%d (%s)
			</div>`, upstreamIP, upstreamPort, strings.ToUpper(upstreamType))
		}

		// Get device stats
		deviceMutex.RLock()
		deviceHTML := ""
		for _, device := range devices {
			deviceHTML += fmt.Sprintf(`
				<tr>
					<td>%s</td>
					<td><code>%s</code></td>
					<td>%s</td>
					<td>%s</td>
					<td>%d</td>
					<td style="font-size:0.85em;color:#888;">%s</td>
				</tr>`,
				device.IP,
				device.MAC,
				formatBytes(atomic.LoadInt64(&device.Upload)),
				formatBytes(atomic.LoadInt64(&device.Download)),
				atomic.LoadInt64(&device.ConnCount),
				device.LastTarget)
		}
		deviceMutex.RUnlock()

		if deviceHTML == "" {
			deviceHTML = "<tr><td colspan='6' style='text-align:center;color:#888;'>No devices connected yet</td></tr>"
		}

		// Get recent connections
		logMutex.Lock()
		connLogsHTML := ""
		logCount := len(recentConns)
		startIdx := 0
		if logCount > 20 {
			startIdx = logCount - 20
		}
		for i := logCount - 1; i >= startIdx; i-- {
			conn := recentConns[i]
			statusIcon := "✓"
			statusColor := "#00ff88"
			if conn.Status != "SUCCESS" {
				statusIcon = "✗"
				statusColor = "#ff5555"
			}
			connLogsHTML += fmt.Sprintf(`
				<tr>
					<td style="font-size:0.85em;">%s</td>
					<td><span style="color:%s;">%s</span></td>
					<td>%s</td>
					<td><code style="font-size:0.8em;">%s</code></td>
					<td style="font-size:0.85em;">%s</td>
					<td>%s</td>
					<td>%s</td>
				</tr>`,
				conn.Time.Format("15:04:05"),
				statusColor, statusIcon,
				conn.Protocol,
				conn.ClientIP,
				conn.Target,
				formatBytes(conn.Upload),
				formatBytes(conn.Download))
		}
		logMutex.Unlock()

		if connLogsHTML == "" {
			connLogsHTML = "<tr><td colspan='7' style='text-align:center;color:#888;'>No connections yet</td></tr>"
		}

		html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <title>VPN Share - Live Monitor</title>
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <meta http-equiv="refresh" content="5">
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
            background: linear-gradient(135deg, #1a1a2e 0%%, #16213e 100%%);
            color: white;
            padding: 20px;
            min-height: 100vh;
        }
        .container { max-width: 1200px; margin: 0 auto; }
        h1 { 
            color: #00ff88; 
            text-align: center;
            margin-bottom: 20px;
            font-size: 2em;
        }
        .success {
            background: #00ff88;
            color: #000;
            padding: 20px;
            border-radius: 10px;
            text-align: center;
            font-weight: bold;
            font-size: 1.3em;
            margin: 15px 0;
            box-shadow: 0 4px 15px rgba(0,255,136,0.3);
        }
        .stats, .devices, .config, .connections {
            background: rgba(255,255,255,0.1);
            padding: 25px;
            border-radius: 10px;
            margin: 20px 0;
            backdrop-filter: blur(10px);
        }
        .stats h3, .devices h3, .config h3, .connections h3 {
            margin-bottom: 15px;
            color: #00ff88;
            border-bottom: 2px solid #00ff88;
            padding-bottom: 10px;
        }
        .stat-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 15px;
            margin-top: 15px;
        }
        .stat-item {
            background: rgba(0,150,255,0.2);
            padding: 15px;
            border-radius: 8px;
            text-align: center;
        }
        .stat-label {
            color: #888;
            font-size: 0.9em;
            margin-bottom: 5px;
        }
        .stat-value {
            font-size: 1.5em;
            font-weight: bold;
            color: #00ff88;
        }
        table {
            width: 100%%;
            border-collapse: collapse;
            margin-top: 15px;
        }
        th, td {
            padding: 12px 8px;
            text-align: left;
            border-bottom: 1px solid rgba(255,255,255,0.1);
        }
        th {
            background: rgba(0,0,0,0.3);
            font-weight: bold;
            color: #00ff88;
            font-size: 0.9em;
        }
        tr:hover {
            background: rgba(255,255,255,0.05);
        }
        code {
            background: rgba(0,0,0,0.5);
            padding: 4px 8px;
            border-radius: 4px;
            font-family: 'Courier New', monospace;
            color: #00ff88;
        }
        .refresh-note {
            text-align: center;
            color: #888;
            font-size: 0.85em;
            margin-top: 20px;
        }
        .config-item {
            display: flex;
            justify-content: space-between;
            padding: 10px 0;
            border-bottom: 1px solid rgba(255,255,255,0.05);
        }
        .config-label {
            color: #888;
        }
        .live-indicator {
            display: inline-block;
            width: 10px;
            height: 10px;
            background: #00ff88;
            border-radius: 50%%;
            animation: pulse 2s infinite;
            margin-right: 5px;
        }
        @keyframes pulse {
            0%%, 100%% { opacity: 1; }
            50%% { opacity: 0.5; }
        }
    </style>
</head>
<body>
    <div class="container">
        <h1><span class="live-indicator"></span>VPN Share - Live Monitor</h1>
        
        <div class="success">
            🟢 Server Active & Monitoring
        </div>
        
        %s
        
        <div class="stats">
            <h3>📊 Transfer Statistics</h3>
            <div class="stat-grid">
                <div class="stat-item">
                    <div class="stat-label">Total Upload</div>
                    <div class="stat-value">%s</div>
                </div>
                <div class="stat-item">
                    <div class="stat-label">Total Download</div>
                    <div class="stat-value">%s</div>
                </div>
                <div class="stat-item">
                    <div class="stat-label">Total Transfer</div>
                    <div class="stat-value">%s</div>
                </div>
            </div>
        </div>

        <div class="connections">
            <h3>🔴 Recent Connections (Last 20)</h3>
            <table>
                <thead>
                    <tr>
                        <th>Time</th>
                        <th>Status</th>
                        <th>Protocol</th>
                        <th>Client IP</th>
                        <th>Target</th>
                        <th>Upload</th>
                        <th>Download</th>
                    </tr>
                </thead>
                <tbody>
                    %s
                </tbody>
            </table>
        </div>
        
        <div class="devices">
            <h3>📱 Connected Devices</h3>
            <table>
                <thead>
                    <tr>
                        <th>IP Address</th>
                        <th>MAC Address</th>
                        <th>Upload</th>
                        <th>Download</th>
                        <th>Conns</th>
                        <th>Last Target</th>
                    </tr>
                </thead>
                <tbody>
                    %s
                </tbody>
            </table>
        </div>
        
        <div class="config">
            <h3>📱 NekoBox Configuration</h3>
            <div class="config-item">
                <span class="config-label">Type:</span>
                <code>SOCKS</code>
            </div>
            <div class="config-item">
                <span class="config-label">Server:</span>
                <code>%s</code>
            </div>
            <div class="config-item">
                <span class="config-label">Port:</span>
                <code>%d</code>
            </div>
            <div class="config-item">
                <span class="config-label">Username:</span>
                <code>(empty)</code>
            </div>
            <div class="config-item">
                <span class="config-label">Password:</span>
                <code>(empty)</code>
            </div>
            <div class="config-item">
                <span class="config-label">TLS:</span>
                <code>OFF</code>
            </div>
        </div>
        
        <div class="config">
            <h3>📱 V2rayNG Configuration</h3>
            <div class="config-item">
                <span class="config-label">Type:</span>
                <code>Socks</code>
            </div>
            <div class="config-item">
                <span class="config-label">Address:</span>
                <code>%s</code>
            </div>
            <div class="config-item">
                <span class="config-label">Port:</span>
                <code>%d</code>
            </div>
        </div>
        
        <p class="refresh-note">⟳ Auto-refresh every 5 seconds</p>
    </div>
</body>
</html>`,
			upstreamStatus,
			formatBytes(up),
			formatBytes(down),
			formatBytes(up+down),
			connLogsHTML,
			deviceHTML,
			primaryIP, socksPort,
			primaryIP, socksPort)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(html))
	})

	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", bindAddr, testPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	log.Printf("[TEST] Server started on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[TEST] Server error: %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// SOCKS5 SERVER
// ═══════════════════════════════════════════════════════════════════════════════

func startSOCKS5Server(ctx context.Context) {
	addr := fmt.Sprintf("%s:%d", bindAddr, socksPort)

	lc := net.ListenConfig{
		KeepAlive: 3 * time.Minute,
	}

	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		log.Fatalf("[SOCKS5] Failed to start: %v", err)
	}
	defer listener.Close()

	log.Printf("[SOCKS5] Server started on %s", addr)

	semaphore := make(chan struct{}, 2000)

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}

		select {
		case semaphore <- struct{}{}:
			go func() {
				defer func() {
					<-semaphore
					if r := recover(); r != nil {
						log.Printf("[SOCKS5] Recovered from panic: %v", r)
					}
				}()
				handleSOCKS5Connection(ctx, conn)
			}()
		default:
			conn.Close()
		}
	}
}

func handleSOCKS5Connection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	atomic.AddInt64(&connections, 1)
	atomic.AddInt64(&activeConns, 1)
	defer atomic.AddInt64(&activeConns, -1)

	connNum := atomic.LoadInt64(&connections)
	clientIP := strings.Split(conn.RemoteAddr().String(), ":")[0]
	clientMAC := getMACAddress(clientIP)

	startTime := time.Now()

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	// SOCKS5 handshake
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}

	if header[0] != 0x05 {
		return
	}

	nmethods := int(header[1])
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}

	supportsNoAuth := false
	for _, m := range methods {
		if m == 0x00 {
			supportsNoAuth = true
			break
		}
	}

	if !supportsNoAuth {
		conn.Write([]byte{0x05, 0xFF})
		return
	}

	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return
	}

	if request[0] != 0x05 || request[1] != 0x01 {
		sendSOCKS5Error(conn, 0x07)
		return
	}

	atyp := request[3]
	var destHost string

	switch atyp {
	case 0x01:
		ipv4 := make([]byte, 4)
		if _, err := io.ReadFull(conn, ipv4); err != nil {
			return
		}
		destHost = net.IP(ipv4).String()

	case 0x03:
		domainLenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, domainLenBuf); err != nil {
			return
		}
		domain := make([]byte, domainLenBuf[0])
		if _, err := io.ReadFull(conn, domain); err != nil {
			return
		}
		destHost = string(domain)

	case 0x04:
		ipv6 := make([]byte, 16)
		if _, err := io.ReadFull(conn, ipv6); err != nil {
			return
		}
		destHost = net.IP(ipv6).String()

	default:
		sendSOCKS5Error(conn, 0x08)
		return
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}
	destPort := binary.BigEndian.Uint16(portBuf)
	destAddr := fmt.Sprintf("%s:%d", destHost, destPort)

	// Update device last target
	deviceMutex.Lock()
	if device, exists := devices[clientIP]; exists {
		device.LastTarget = destAddr
	}
	deviceMutex.Unlock()

	var targetConn net.Conn
	var err error

	if upstreamEnable {
		if upstreamType == "socks5" {
			targetConn, err = connectViaSOCKS5(upstreamIP, upstreamPort, destHost, destPort, atyp)
		} else {
			targetConn, err = connectViaSOCKS5(upstreamIP, upstreamPort, destHost, destPort, atyp)
		}
	} else {
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 3 * time.Minute,
		}
		targetConn, err = dialer.DialContext(ctx, "tcp", destAddr)
	}

	connStatus := "SUCCESS"
	if err != nil {
		connStatus = "FAILED"
		sendSOCKS5Error(conn, 0x01)
		
		logConnection(ConnectionLog{
			Time:      startTime,
			ConnID:    connNum,
			ClientIP:  clientIP,
			ClientMAC: clientMAC,
			Target:    destAddr,
			Protocol:  "SOCKS5",
			Status:    connStatus,
			Upload:    0,
			Download:  0,
		})
		return
	}
	defer targetConn.Close()

	localAddr := targetConn.LocalAddr().(*net.TCPAddr)
	response := make([]byte, 10)
	response[0] = 0x05
	response[1] = 0x00
	response[2] = 0x00
	response[3] = 0x01

	ip4 := localAddr.IP.To4()
	if ip4 != nil {
		copy(response[4:8], ip4)
	}
	binary.BigEndian.PutUint16(response[8:10], uint16(localAddr.Port))

	if _, err := conn.Write(response); err != nil {
		return
	}

	conn.SetDeadline(time.Time{})
	targetConn.SetDeadline(time.Time{})

	uploaded, downloaded := relay(ctx, conn, targetConn)
	atomic.AddInt64(&totalUpload, uploaded)
	atomic.AddInt64(&totalDownload, downloaded)
	trackDevice(clientIP, uploaded, downloaded)

	logConnection(ConnectionLog{
		Time:      startTime,
		ConnID:    connNum,
		ClientIP:  clientIP,
		ClientMAC: clientMAC,
		Target:    destAddr,
		Protocol:  "SOCKS5",
		Status:    connStatus,
		Upload:    uploaded,
		Download:  downloaded,
	})
}

func connectViaSOCKS5(host string, port int, destHost string, destPort uint16, atyp byte) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout: 30 * time.Second,
	}

	upstreamAddr := fmt.Sprintf("%s:%d", host, port)
	upstreamConn, err := dialer.Dial("tcp", upstreamAddr)
	if err != nil {
		return nil, fmt.Errorf("upstream connect failed: %v", err)
	}

	if _, err := upstreamConn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		upstreamConn.Close()
		return nil, err
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(upstreamConn, resp); err != nil || resp[1] != 0x00 {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream auth failed")
	}

	var req []byte
	req = append(req, 0x05, 0x01, 0x00)

	if atyp == 0x03 {
		req = append(req, 0x03, byte(len(destHost)))
		req = append(req, []byte(destHost)...)
	} else if atyp == 0x01 {
		req = append(req, 0x01)
		ip := net.ParseIP(destHost).To4()
		if ip == nil {
			upstreamConn.Close()
			return nil, fmt.Errorf("invalid IPv4")
		}
		req = append(req, ip...)
	} else {
		req = append(req, 0x04)
		ip := net.ParseIP(destHost).To16()
		if ip == nil {
			upstreamConn.Close()
			return nil, fmt.Errorf("invalid IPv6")
		}
		req = append(req, ip...)
	}

	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, destPort)
	req = append(req, portBytes...)

	if _, err := upstreamConn.Write(req); err != nil {
		upstreamConn.Close()
		return nil, err
	}

	respHeader := make([]byte, 4)
	if _, err := io.ReadFull(upstreamConn, respHeader); err != nil {
		upstreamConn.Close()
		return nil, err
	}

	if respHeader[1] != 0x00 {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream connect failed: code %d", respHeader[1])
	}

	switch respHeader[3] {
	case 0x01:
		io.ReadFull(upstreamConn, make([]byte, 4))
	case 0x03:
		lenByte := make([]byte, 1)
		io.ReadFull(upstreamConn, lenByte)
		io.ReadFull(upstreamConn, make([]byte, lenByte[0]))
	case 0x04:
		io.ReadFull(upstreamConn, make([]byte, 16))
	}
	io.ReadFull(upstreamConn, make([]byte, 2))

	return upstreamConn, nil
}

func relay(ctx context.Context, client, target net.Conn) (uploaded, downloaded int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	done := make(chan struct{}, 1)

	go func() {
		defer wg.Done()
		uploaded = copyWithBuffer(target, client)
		if tc, ok := target.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		downloaded = copyWithBuffer(client, target)
		if tc, ok := client.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		client.Close()
		target.Close()
	case <-done:
	}

	wg.Wait()
	return
}

func copyWithBuffer(dst io.Writer, src io.Reader) int64 {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	buf := *bufPtr

	var total int64
	for {
		nr, readErr := src.Read(buf)
		if nr > 0 {
			nw, writeErr := dst.Write(buf[:nr])
			if nw > 0 {
				total += int64(nw)
			}
			if writeErr != nil {
				break
			}
		}
		if readErr != nil {
			break
		}
	}
	return total
}

func sendSOCKS5Error(conn net.Conn, errCode byte) {
	response := []byte{0x05, errCode, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	conn.Write(response)
}

// ═══════════════════════════════════════════════════════════════════════════════
// HTTP PROXY SERVER
// ═══════════════════════════════════════════════════════════════════════════════

func startHTTPProxy(ctx context.Context) {
	addr := fmt.Sprintf("%s:%d", bindAddr, httpPort)

	lc := net.ListenConfig{
		KeepAlive: 3 * time.Minute,
	}

	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		log.Fatalf("[HTTP] Failed to start: %v", err)
	}
	defer listener.Close()

	log.Printf("[HTTP] Server started on %s", addr)

	semaphore := make(chan struct{}, 2000)

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}

		select {
		case semaphore <- struct{}{}:
			go func() {
				defer func() {
					<-semaphore
					if r := recover(); r != nil {
						log.Printf("[HTTP] Recovered from panic: %v", r)
					}
				}()
				handleHTTPConnection(ctx, conn)
			}()
		default:
			conn.Close()
		}
	}
}

func handleHTTPConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	atomic.AddInt64(&activeConns, 1)
	defer atomic.AddInt64(&activeConns, -1)

	clientIP := strings.Split(conn.RemoteAddr().String(), ":")[0]
	clientMAC := getMACAddress(clientIP)

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}

	startTime := time.Now()
	connNum := atomic.LoadInt64(&connections)
	atomic.AddInt64(&connections, 1)

	if req.Method == http.MethodConnect {
		handleHTTPSConnect(ctx, conn, req, clientIP, clientMAC, startTime, connNum)
	} else {
		handleHTTPForward(ctx, conn, req, clientIP, clientMAC, startTime, connNum)
	}
}

func handleHTTPSConnect(ctx context.Context, conn net.Conn, req *http.Request, clientIP, clientMAC string, startTime time.Time, connNum int64) {
	target := req.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}

	// Update device last target
	deviceMutex.Lock()
	if device, exists := devices[clientIP]; exists {
		device.LastTarget = target
	}
	deviceMutex.Unlock()

	var targetConn net.Conn
	var err error

	if upstreamEnable {
		host, portStr, _ := net.SplitHostPort(target)
		port := 443
		fmt.Sscanf(portStr, "%d", &port)

		if upstreamType == "socks5" {
			targetConn, err = connectViaSOCKS5(upstreamIP, upstreamPort, host, uint16(port), 0x03)
		} else {
			dialer := &net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 3 * time.Minute,
			}
			targetConn, err = dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", upstreamIP, upstreamPort))
			if err == nil {
				connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
				targetConn.Write([]byte(connectReq))

				bufReader := bufio.NewReader(targetConn)
				resp, err := http.ReadResponse(bufReader, req)
				if err != nil || resp.StatusCode != 200 {
					targetConn.Close()
					conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
					
					logConnection(ConnectionLog{
						Time:      startTime,
						ConnID:    connNum,
						ClientIP:  clientIP,
						ClientMAC: clientMAC,
						Target:    target,
						Protocol:  "HTTP",
						Status:    "FAILED",
						Upload:    0,
						Download:  0,
					})
					return
				}
			}
		}
	} else {
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 3 * time.Minute,
		}
		targetConn, err = dialer.DialContext(ctx, "tcp", target)
	}

	if err != nil {
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		
		logConnection(ConnectionLog{
			Time:      startTime,
			ConnID:    connNum,
			ClientIP:  clientIP,
			ClientMAC: clientMAC,
			Target:    target,
			Protocol:  "HTTP",
			Status:    "FAILED",
			Upload:    0,
			Download:  0,
		})
		return
	}
	defer targetConn.Close()

	conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	conn.SetDeadline(time.Time{})
	targetConn.SetDeadline(time.Time{})

	uploaded, downloaded := relay(ctx, conn, targetConn)
	atomic.AddInt64(&totalUpload, uploaded)
	atomic.AddInt64(&totalDownload, downloaded)
	trackDevice(clientIP, uploaded, downloaded)

	logConnection(ConnectionLog{
		Time:      startTime,
		ConnID:    connNum,
		ClientIP:  clientIP,
		ClientMAC: clientMAC,
		Target:    target,
		Protocol:  "HTTP",
		Status:    "SUCCESS",
		Upload:    uploaded,
		Download:  downloaded,
	})
}

func handleHTTPForward(ctx context.Context, conn net.Conn, req *http.Request, clientIP, clientMAC string, startTime time.Time, connNum int64) {
	target := req.Host
	if !strings.Contains(target, ":") {
		target += ":80"
	}

	// Update device last target
	deviceMutex.Lock()
	if device, exists := devices[clientIP]; exists {
		device.LastTarget = target
	}
	deviceMutex.Unlock()

	var targetConn net.Conn
	var err error

	if upstreamEnable && upstreamType == "socks5" {
		host, portStr, _ := net.SplitHostPort(target)
		port := 80
		fmt.Sscanf(portStr, "%d", &port)
		targetConn, err = connectViaSOCKS5(upstreamIP, upstreamPort, host, uint16(port), 0x03)
	} else {
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 3 * time.Minute,
		}
		targetConn, err = dialer.DialContext(ctx, "tcp", target)
	}

	if err != nil {
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		
		logConnection(ConnectionLog{
			Time:      startTime,
			ConnID:    connNum,
			ClientIP:  clientIP,
			ClientMAC: clientMAC,
			Target:    target,
			Protocol:  "HTTP",
			Status:    "FAILED",
			Upload:    0,
			Download:  0,
		})
		return
	}
	defer targetConn.Close()

	req.Header.Del("Proxy-Connection")
	req.Write(targetConn)

	downloaded, _ := io.Copy(conn, targetConn)
	atomic.AddInt64(&totalDownload, downloaded)
	trackDevice(clientIP, 0, downloaded)

	logConnection(ConnectionLog{
		Time:      startTime,
		ConnID:    connNum,
		ClientIP:  clientIP,
		ClientMAC: clientMAC,
		Target:    target,
		Protocol:  "HTTP",
		Status:    "SUCCESS",
		Upload:    0,
		Download:  downloaded,
	})
}
