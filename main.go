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
	socksPort     int
	httpPort      int
	testPort      int
	bindAddr      string
	verbose       bool
	useTor        bool
	torPort       int
	connections   int64
	activeConns   int64
	totalUpload   int64
	totalDownload int64

	// Buffer pool for better memory usage
	bufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 32*1024) // 32KB buffer
			return &buf
		},
	}
)

func main() {
	flag.IntVar(&socksPort, "socks", 10808, "SOCKS5 port")
	flag.IntVar(&httpPort, "http", 10809, "HTTP proxy port")
	flag.IntVar(&testPort, "test", 10810, "Test HTTP server port")
	flag.StringVar(&bindAddr, "bind", "0.0.0.0", "Bind address")
	flag.BoolVar(&verbose, "v", false, "Verbose logging")
	flag.BoolVar(&useTor, "tor", false, "Use Tor Browser as upstream (port 9150)")
	flag.IntVar(&torPort, "tor-port", 9150, "Tor Browser SOCKS5 port")
	flag.Parse()

	verbose = true

	clearScreen()
	printBanner()

	ips := getLocalIPs()
	primaryIP := "127.0.0.1"
	if len(ips) > 0 {
		primaryIP = ips[0]
	}

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              VPN SHARE FOR NEKOBOX / V2RAYNG                 ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	fmt.Println("Your IP addresses:")
	for _, ip := range ips {
		fmt.Printf("  → %s\n", ip)
	}
	fmt.Println()

	if useTor {
		fmt.Printf("🧅 Tor Mode ENABLED - Upstream: 127.0.0.1:%d\n", torPort)
		// Test Tor connection
		if testTorConnection() {
			fmt.Println("   ✓ Tor Browser connection OK")
		} else {
			fmt.Println("   ✗ WARNING: Cannot connect to Tor Browser!")
			fmt.Println("   Make sure Tor Browser is running.")
		}
		fmt.Println()
	}

	// Disable firewall on Windows
	if runtime.GOOS == "windows" {
		exec.Command("netsh", "advfirewall", "set", "allprofiles", "state", "off").Run()
		fmt.Println("Firewall disabled")
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

	time.Sleep(500 * time.Millisecond)

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    ✅ SERVERS RUNNING                        ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  SOCKS5:     %s:%-39d  ║\n", primaryIP, socksPort)
	fmt.Printf("║  HTTP:       %s:%-39d  ║\n", primaryIP, httpPort)
	fmt.Printf("║  Test Page:  http://%s:%-30d  ║\n", primaryIP, testPort)
	if useTor {
		fmt.Println("╠══════════════════════════════════════════════════════════════╣")
		fmt.Printf("║  🧅 Tor Upstream: 127.0.0.1:%-33d  ║\n", torPort)
	}
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Println("║  NekoBox: + → Manual → SOCKS                                 ║")
	fmt.Printf("║           Server: %s  Port: %d                     ║\n", primaryIP, socksPort)
	fmt.Println("║           (Leave username/password EMPTY)                    ║")
	fmt.Println("║           (Make sure TLS is OFF)                             ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("Waiting for connections... (Ctrl+C to stop)")
	fmt.Println()
	fmt.Println("─────────────────────────────────────────────────────────────────")
	fmt.Println()

	// Wait for exit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\n\nShutting down...")
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
		fmt.Println("All servers stopped.")
	case <-time.After(5 * time.Second):
		fmt.Println("Timeout waiting for servers, forcing exit.")
	}
}

func testTorConnection() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", torPort), 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func displayStats(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			up := atomic.LoadInt64(&totalUpload)
			down := atomic.LoadInt64(&totalDownload)
			active := atomic.LoadInt64(&activeConns)
			total := atomic.LoadInt64(&connections)

			fmt.Printf("\r📊 Stats | Active: %d | Total: %d | ↑ %s | ↓ %s | Total: %s     ",
				active, total,
				formatBytes(up),
				formatBytes(down),
				formatBytes(up+down))
		}
	}
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
		log.Printf("[TEST] ✓ Browser connection from %s", r.RemoteAddr)

		up := atomic.LoadInt64(&totalUpload)
		down := atomic.LoadInt64(&totalDownload)
		torStatus := ""
		if useTor {
			torStatus = `<div class="success" style="background: #ff9800;">🧅 Tor Mode Active</div>`
		}

		html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <title>VPN Share - Working!</title>
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <meta http-equiv="refresh" content="5">
    <style>
        body {
            font-family: -apple-system, sans-serif;
            background: linear-gradient(135deg, #1a1a2e, #16213e);
            color: white;
            margin: 0;
            padding: 20px;
            min-height: 100vh;
        }
        .container { max-width: 500px; margin: 0 auto; }
        h1 { color: #00ff88; text-align: center; }
        .success {
            background: #00ff88;
            color: black;
            padding: 20px;
            border-radius: 10px;
            text-align: center;
            font-weight: bold;
            font-size: 1.2em;
            margin: 10px 0;
        }
        .stats {
            background: rgba(0,150,255,0.3);
            padding: 20px;
            border-radius: 10px;
            margin: 20px 0;
            text-align: center;
        }
        .config {
            background: rgba(255,255,255,0.1);
            padding: 20px;
            border-radius: 10px;
            margin: 20px 0;
        }
        code {
            background: rgba(0,0,0,0.3);
            padding: 2px 8px;
            border-radius: 4px;
        }
        table { width: 100%%; }
        td { padding: 8px 0; }
        .label { color: #888; }
        .big { font-size: 1.5em; font-weight: bold; }
    </style>
</head>
<body>
    <div class="container">
        <h1>✅ VPN Share Working!</h1>
        <div class="success">Network Connection OK!</div>
        %s
        <div class="stats">
            <h3>📊 Transfer Statistics</h3>
            <p>↑ Upload: <span class="big">%s</span></p>
            <p>↓ Download: <span class="big">%s</span></p>
            <p>Total: <span class="big">%s</span></p>
            <p style="color:#888;font-size:0.8em;">Auto-refresh every 5 seconds</p>
        </div>
        <div class="config">
            <h3>📱 NekoBox Settings:</h3>
            <table>
                <tr><td class="label">Type:</td><td><code>SOCKS</code></td></tr>
                <tr><td class="label">Server:</td><td><code>%s</code></td></tr>
                <tr><td class="label">Port:</td><td><code>%d</code></td></tr>
                <tr><td class="label">Username:</td><td><code>(empty)</code></td></tr>
                <tr><td class="label">Password:</td><td><code>(empty)</code></td></tr>
                <tr><td class="label">TLS:</td><td><code>OFF</code></td></tr>
            </table>
        </div>
        <div class="config">
            <h3>📱 V2rayNG Settings:</h3>
            <table>
                <tr><td class="label">Type:</td><td><code>Socks</code></td></tr>
                <tr><td class="label">Address:</td><td><code>%s</code></td></tr>
                <tr><td class="label">Port:</td><td><code>%d</code></td></tr>
            </table>
        </div>
    </div>
</body>
</html>`, torStatus, formatBytes(up), formatBytes(down), formatBytes(up+down),
			primaryIP, socksPort, primaryIP, socksPort)

		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))
	})

	// Stats API endpoint
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		up := atomic.LoadInt64(&totalUpload)
		down := atomic.LoadInt64(&totalDownload)
		active := atomic.LoadInt64(&activeConns)
		total := atomic.LoadInt64(&connections)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"upload":%d,"download":%d,"active":%d,"total":%d}`,
			up, down, active, total)
	})

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", bindAddr, testPort),
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	log.Printf("[TEST] Server started on %s", server.Addr)
	server.ListenAndServe()
}

// ═══════════════════════════════════════════════════════════════════════════════
// IMPROVED ASYNC SOCKS5 SERVER
// ═══════════════════════════════════════════════════════════════════════════════

func startSOCKS5Server(ctx context.Context) {
	addr := fmt.Sprintf("%s:%d", bindAddr, socksPort)
	
	lc := net.ListenConfig{
		KeepAlive: 30 * time.Second,
	}
	
	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		log.Fatalf("[SOCKS5] Failed to start: %v", err)
	}
	defer listener.Close()
	
	log.Printf("[SOCKS5] Server started on %s", addr)

	// Worker pool for handling connections
	connChan := make(chan net.Conn, 1000)
	
	// Start worker goroutines
	numWorkers := runtime.NumCPU() * 2
	for i := 0; i < numWorkers; i++ {
		go socksWorker(ctx, connChan)
	}

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
		case connChan <- conn:
		default:
			// Channel full, handle directly
			go handleSOCKS5Connection(ctx, conn)
		}
	}
}

func socksWorker(ctx context.Context, connChan <-chan net.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case conn := <-connChan:
			handleSOCKS5Connection(ctx, conn)
		}
	}
}

func handleSOCKS5Connection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	atomic.AddInt64(&connections, 1)
	atomic.AddInt64(&activeConns, 1)
	defer atomic.AddInt64(&activeConns, -1)

	connNum := atomic.LoadInt64(&connections)
	clientAddr := conn.RemoteAddr().String()
	
	if verbose {
		log.Printf("[SOCKS5] #%d New connection from %s", connNum, clientAddr)
	}

	// Set reasonable timeout
	conn.SetDeadline(time.Now().Add(2 * time.Minute))

	// Read client greeting
	header := make([]byte, 2)
	n, err := io.ReadFull(conn, header)
	if err != nil {
		if verbose {
			log.Printf("[SOCKS5] #%d Failed to read header: %v (read %d bytes)", connNum, err, n)
		}
		return
	}

	version := header[0]
	nmethods := int(header[1])

	if version != 0x05 {
		if verbose {
			log.Printf("[SOCKS5] #%d Invalid SOCKS version: %d", connNum, version)
		}
		return
	}

	// Read methods
	methods := make([]byte, nmethods)
	_, err = io.ReadFull(conn, methods)
	if err != nil {
		return
	}

	// Check for no-auth support
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

	// Send no-auth response
	_, err = conn.Write([]byte{0x05, 0x00})
	if err != nil {
		return
	}

	// Read connection request
	request := make([]byte, 4)
	_, err = io.ReadFull(conn, request)
	if err != nil {
		return
	}

	ver := request[0]
	cmd := request[1]
	atyp := request[3]

	if ver != 0x05 {
		sendSOCKS5Error(conn, 0x01)
		return
	}

	if cmd != 0x01 {
		sendSOCKS5Error(conn, 0x07)
		return
	}

	// Parse destination address
	var destHost string

	switch atyp {
	case 0x01: // IPv4
		ipv4 := make([]byte, 4)
		_, err = io.ReadFull(conn, ipv4)
		if err != nil {
			return
		}
		destHost = net.IP(ipv4).String()

	case 0x03: // Domain
		domainLenBuf := make([]byte, 1)
		_, err = io.ReadFull(conn, domainLenBuf)
		if err != nil {
			return
		}
		domain := make([]byte, domainLenBuf[0])
		_, err = io.ReadFull(conn, domain)
		if err != nil {
			return
		}
		destHost = string(domain)

	case 0x04: // IPv6
		ipv6 := make([]byte, 16)
		_, err = io.ReadFull(conn, ipv6)
		if err != nil {
			return
		}
		destHost = net.IP(ipv6).String()

	default:
		sendSOCKS5Error(conn, 0x08)
		return
	}

	// Read port
	portBuf := make([]byte, 2)
	_, err = io.ReadFull(conn, portBuf)
	if err != nil {
		return
	}
	destPort := binary.BigEndian.Uint16(portBuf)
	destAddr := fmt.Sprintf("%s:%d", destHost, destPort)

	if verbose {
		log.Printf("[SOCKS5] #%d → CONNECT to %s", connNum, destAddr)
	}

	// Connect to destination (directly or through Tor)
	var targetConn net.Conn

	if useTor {
		targetConn, err = connectViaTor(ctx, destAddr, atyp, destHost, destPort)
	} else {
		dialer := &net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		targetConn, err = dialer.DialContext(ctx, "tcp", destAddr)
	}

	if err != nil {
		if verbose {
			log.Printf("[SOCKS5] #%d ✗ Failed to connect to %s: %v", connNum, destAddr, err)
		}
		if strings.Contains(err.Error(), "refused") {
			sendSOCKS5Error(conn, 0x05)
		} else if strings.Contains(err.Error(), "timeout") {
			sendSOCKS5Error(conn, 0x04)
		} else {
			sendSOCKS5Error(conn, 0x01)
		}
		return
	}
	defer targetConn.Close()

	if verbose {
		log.Printf("[SOCKS5] #%d ✓ Connected to %s", connNum, destAddr)
	}

	// Send success response
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

	_, err = conn.Write(response)
	if err != nil {
		return
	}

	// Clear deadlines for relay
	conn.SetDeadline(time.Time{})
	targetConn.SetDeadline(time.Time{})

	// Async bidirectional copy with stats
	uploaded, downloaded := relayAsync(ctx, conn, targetConn)

	// Update global stats
	atomic.AddInt64(&totalUpload, uploaded)
	atomic.AddInt64(&totalDownload, downloaded)

	if verbose {
		log.Printf("[SOCKS5] #%d ✓ Closed: %s (↑%s ↓%s)",
			connNum, destAddr, formatBytes(uploaded), formatBytes(downloaded))
	}
}

func connectViaTor(ctx context.Context, destAddr string, atyp byte, destHost string, destPort uint16) (net.Conn, error) {
	// Connect to Tor SOCKS5 proxy
	dialer := &net.Dialer{
		Timeout: 30 * time.Second,
	}
	
	torAddr := fmt.Sprintf("127.0.0.1:%d", torPort)
	torConn, err := dialer.DialContext(ctx, "tcp", torAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Tor: %v", err)
	}

	// SOCKS5 handshake with Tor
	// Send greeting
	_, err = torConn.Write([]byte{0x05, 0x01, 0x00})
	if err != nil {
		torConn.Close()
		return nil, err
	}

	// Read response
	resp := make([]byte, 2)
	_, err = io.ReadFull(torConn, resp)
	if err != nil || resp[1] != 0x00 {
		torConn.Close()
		return nil, fmt.Errorf("Tor auth failed")
	}

	// Send connect request
	var req []byte
	req = append(req, 0x05, 0x01, 0x00) // VER, CMD, RSV

	if atyp == 0x03 { // Domain
		req = append(req, 0x03, byte(len(destHost)))
		req = append(req, []byte(destHost)...)
	} else if atyp == 0x01 { // IPv4
		req = append(req, 0x01)
		ip := net.ParseIP(destHost).To4()
		req = append(req, ip...)
	} else { // IPv6
		req = append(req, 0x04)
		ip := net.ParseIP(destHost).To16()
		req = append(req, ip...)
	}

	// Port
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, destPort)
	req = append(req, portBytes...)

	_, err = torConn.Write(req)
	if err != nil {
		torConn.Close()
		return nil, err
	}

	// Read Tor response
	respHeader := make([]byte, 4)
	_, err = io.ReadFull(torConn, respHeader)
	if err != nil {
		torConn.Close()
		return nil, err
	}

	if respHeader[1] != 0x00 {
		torConn.Close()
		return nil, fmt.Errorf("Tor connect failed: %d", respHeader[1])
	}

	// Skip bound address
	switch respHeader[3] {
	case 0x01:
		io.ReadFull(torConn, make([]byte, 4))
	case 0x03:
		lenByte := make([]byte, 1)
		io.ReadFull(torConn, lenByte)
		io.ReadFull(torConn, make([]byte, lenByte[0]))
	case 0x04:
		io.ReadFull(torConn, make([]byte, 16))
	}
	io.ReadFull(torConn, make([]byte, 2)) // port

	return torConn, nil
}

func relayAsync(ctx context.Context, client, target net.Conn) (uploaded, downloaded int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	// Use buffered channels for async notification
	done := make(chan struct{}, 2)

	// Client -> Target (upload)
	go func() {
		defer wg.Done()
		uploaded = copyBuffer(target, client)
		if tc, ok := target.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	// Target -> Client (download)
	go func() {
		defer wg.Done()
		downloaded = copyBuffer(client, target)
		if tc, ok := client.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	// Wait for completion or context cancellation
	select {
	case <-ctx.Done():
		client.Close()
		target.Close()
	case <-done:
		// First direction finished, wait for the other
		<-done
	}

	wg.Wait()
	return
}

func copyBuffer(dst io.Writer, src io.Reader) int64 {
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
// ASYNC HTTP PROXY SERVER
// ═══════════════════════════════════════════════════════════════════════════════

func startHTTPProxy(ctx context.Context) {
	addr := fmt.Sprintf("%s:%d", bindAddr, httpPort)
	
	lc := net.ListenConfig{
		KeepAlive: 30 * time.Second,
	}
	
	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		log.Fatalf("[HTTP] Failed to start: %v", err)
	}
	defer listener.Close()
	
	log.Printf("[HTTP] Server started on %s", addr)

	// Worker pool
	connChan := make(chan net.Conn, 1000)
	numWorkers := runtime.NumCPU() * 2
	
	for i := 0; i < numWorkers; i++ {
		go httpWorker(ctx, connChan)
	}

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
		case connChan <- conn:
		default:
			go handleHTTPConnection(ctx, conn)
		}
	}
}

func httpWorker(ctx context.Context, connChan <-chan net.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case conn := <-connChan:
			handleHTTPConnection(ctx, conn)
		}
	}
}

func handleHTTPConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	atomic.AddInt64(&activeConns, 1)
	defer atomic.AddInt64(&activeConns, -1)

	clientAddr := conn.RemoteAddr().String()
	if verbose {
		log.Printf("[HTTP] Connection from %s", clientAddr)
	}

	conn.SetDeadline(time.Now().Add(2 * time.Minute))

	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}

	if verbose {
		log.Printf("[HTTP] %s %s from %s", req.Method, req.Host, clientAddr)
	}

	if req.Method == http.MethodConnect {
		handleHTTPSConnect(ctx, conn, req)
	} else {
		handleHTTPForward(ctx, conn, req)
	}
}

func handleHTTPSConnect(ctx context.Context, conn net.Conn, req *http.Request) {
	target := req.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}

	var targetConn net.Conn
	var err error

	if useTor {
		// Parse host and port for Tor
		host, portStr, _ := net.SplitHostPort(target)
		port := 443
		fmt.Sscanf(portStr, "%d", &port)
		targetConn, err = connectViaTor(ctx, target, 0x03, host, uint16(port))
	} else {
		dialer := &net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		targetConn, err = dialer.DialContext(ctx, "tcp", target)
	}

	if err != nil {
		if verbose {
			log.Printf("[HTTP] CONNECT to %s failed: %v", target, err)
		}
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	
	if verbose {
		log.Printf("[HTTP] ✓ CONNECT tunnel to %s", target)
	}

	conn.SetDeadline(time.Time{})
	targetConn.SetDeadline(time.Time{})

	uploaded, downloaded := relayAsync(ctx, conn, targetConn)
	atomic.AddInt64(&totalUpload, uploaded)
	atomic.AddInt64(&totalDownload, downloaded)
}

func handleHTTPForward(ctx context.Context, conn net.Conn, req *http.Request) {
	target := req.Host
	if !strings.Contains(target, ":") {
		target += ":80"
	}

	var targetConn net.Conn
	var err error

	if useTor {
		host, portStr, _ := net.SplitHostPort(target)
		port := 80
		fmt.Sscanf(portStr, "%d", &port)
		targetConn, err = connectViaTor(ctx, target, 0x03, host, uint16(port))
	} else {
		dialer := &net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		targetConn, err = dialer.DialContext(ctx, "tcp", target)
	}

	if err != nil {
		if verbose {
			log.Printf("[HTTP] Forward to %s failed: %v", target, err)
		}
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	req.Header.Del("Proxy-Connection")
	req.Write(targetConn)
	
	n, _ := io.Copy(conn, targetConn)
	atomic.AddInt64(&totalDownload, n)
}
