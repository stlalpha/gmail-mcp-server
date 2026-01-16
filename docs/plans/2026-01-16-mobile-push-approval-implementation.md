# Mobile Push Approval Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace TOTP with mobile push notifications via ntfy.sh, running in a separate daemon process for true agent isolation.

**Architecture:** Two binaries - the existing MCP server communicates via Unix socket with a new approval daemon. The daemon handles ntfy.sh notifications and polls for responses. Agent cannot access daemon's secrets.

**Tech Stack:** Go, ntfy.sh API, Unix sockets, QR code generation (skip64/go-qrcode)

---

## Task 1: Create Daemon Binary Skeleton

**Files:**
- Create: `cmd/approval-daemon/main.go`
- Modify: `go.mod` (add qrcode dependency)

**Step 1: Create directory structure**

```bash
mkdir -p cmd/approval-daemon
```

**Step 2: Create minimal daemon entry point**

Create `cmd/approval-daemon/main.go`:

```go
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
)

func main() {
	reset := flag.Bool("reset", false, "Reset configuration and re-run setup")
	status := flag.Bool("status", false, "Show daemon status")
	flag.Parse()

	if *status {
		showStatus()
		return
	}

	if *reset {
		resetConfig()
	}

	if err := run(); err != nil {
		log.Fatalf("Daemon error: %v", err)
	}
}

func showStatus() {
	fmt.Println("Status: not implemented yet")
}

func resetConfig() {
	configPath := getConfigPath()
	if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		log.Printf("Warning: could not remove config: %v", err)
	}
	log.Println("Configuration reset. Setup will run on next start.")
}

func getConfigPath() string {
	home, _ := os.UserHomeDir()
	return home + "/.config/gmail-mcp/approval-daemon.json"
}

func run() error {
	log.Println("Approval daemon starting...")
	return nil
}
```

**Step 3: Add QR code dependency**

```bash
go get github.com/skip2/go-qrcode
```

**Step 4: Verify it builds**

```bash
go build -o gmail-approval-daemon ./cmd/approval-daemon
./gmail-approval-daemon --status
```

Expected: "Status: not implemented yet"

**Step 5: Commit**

```bash
git add cmd/approval-daemon/main.go go.mod go.sum
git commit -m "feat: create approval daemon binary skeleton

Adds basic CLI structure with --reset and --status flags.
"
```

---

## Task 2: Implement Configuration Management

**Files:**
- Create: `cmd/approval-daemon/config.go`

**Step 1: Create config structure and load/save**

Create `cmd/approval-daemon/config.go`:

```go
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	NtfyTopic     string `json:"ntfy_topic"`
	SigningSecret string `json:"signing_secret"`
	SetupComplete bool   `json:"setup_complete"`
}

func loadConfig() (*Config, error) {
	configPath := getConfigPath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No config = needs setup
		}
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	return &config, nil
}

func saveConfig(config *Config) error {
	configPath := getConfigPath()

	// Ensure directory exists
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}
	return nil
}

func generateRandomString(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(bytes)[:length], nil
}

func createNewConfig() (*Config, error) {
	topic, err := generateRandomString(32)
	if err != nil {
		return nil, fmt.Errorf("failed to generate topic: %w", err)
	}

	secret, err := generateRandomString(32)
	if err != nil {
		return nil, fmt.Errorf("failed to generate secret: %w", err)
	}

	return &Config{
		NtfyTopic:     "gmail-mcp-" + topic,
		SigningSecret: secret,
		SetupComplete: false,
	}, nil
}
```

**Step 2: Verify it compiles**

```bash
go build -o gmail-approval-daemon ./cmd/approval-daemon
```

Expected: No errors

**Step 3: Commit**

```bash
git add cmd/approval-daemon/config.go
git commit -m "feat: add config management for approval daemon

Handles loading, saving, and generating random topics/secrets.
Config stored at ~/.config/gmail-mcp/approval-daemon.json
"
```

---

## Task 3: Implement Setup Web Server with QR Code

**Files:**
- Create: `cmd/approval-daemon/setup.go`
- Modify: `cmd/approval-daemon/main.go`

**Step 1: Create setup server**

Create `cmd/approval-daemon/setup.go`:

```go
package main

import (
	"context"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"

	qrcode "github.com/skip2/go-qrcode"
)

type SetupServer struct {
	config   *Config
	listener net.Listener
	server   *http.Server
	done     chan bool
}

func newSetupServer(config *Config) (*SetupServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("failed to create listener: %w", err)
	}

	return &SetupServer{
		config:   config,
		listener: listener,
		done:     make(chan bool),
	}, nil
}

func (s *SetupServer) run() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleSetup)
	mux.HandleFunc("/test", s.handleTest)
	mux.HandleFunc("/complete", s.handleComplete)

	s.server = &http.Server{Handler: mux}

	url := fmt.Sprintf("http://%s", s.listener.Addr().String())
	log.Println("═══════════════════════════════════════════════════════════════")
	log.Println("📱 APPROVAL DAEMON SETUP")
	log.Println("═══════════════════════════════════════════════════════════════")
	log.Printf("   Open this URL to complete setup: %s", url)
	log.Println("═══════════════════════════════════════════════════════════════")

	// Try to open browser
	openBrowser(url)

	go s.server.Serve(s.listener)
	<-s.done
	return s.server.Shutdown(context.Background())
}

func (s *SetupServer) handleSetup(w http.ResponseWriter, r *http.Request) {
	// Generate QR code for ntfy topic subscription
	ntfyURL := fmt.Sprintf("ntfy://%s", s.config.NtfyTopic)
	qr, err := qrcode.Encode(ntfyURL, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "Failed to generate QR code", 500)
		return
	}
	qrBase64 := base64.StdEncoding.EncodeToString(qr)

	tmpl := template.Must(template.New("setup").Parse(setupHTML))
	tmpl.Execute(w, map[string]string{
		"Topic":    s.config.NtfyTopic,
		"QRCode":   qrBase64,
	})
}

func (s *SetupServer) handleTest(w http.ResponseWriter, r *http.Request) {
	// Send test notification
	err := sendNtfyNotification(s.config.NtfyTopic, "Test Notification", "If you see this, setup is working!")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fmt.Sprintf(`{"success": false, "error": "%s"}`, err.Error())))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success": true}`))
}

func (s *SetupServer) handleComplete(w http.ResponseWriter, r *http.Request) {
	s.config.SetupComplete = true
	if err := saveConfig(s.config); err != nil {
		http.Error(w, "Failed to save config", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success": true}`))
	s.done <- true
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	}
	if cmd != nil {
		cmd.Start()
	}
}

const setupHTML = `<!DOCTYPE html>
<html>
<head>
    <title>Gmail Approval Daemon Setup</title>
    <style>
        body { font-family: -apple-system, system-ui, sans-serif; max-width: 600px; margin: 50px auto; padding: 20px; }
        h1 { color: #333; }
        .qr-container { text-align: center; margin: 30px 0; }
        .topic { font-family: monospace; background: #f5f5f5; padding: 10px; border-radius: 4px; word-break: break-all; }
        .btn { background: #4CAF50; color: white; border: none; padding: 12px 24px; border-radius: 4px; cursor: pointer; font-size: 16px; margin: 5px; }
        .btn:disabled { background: #ccc; cursor: not-allowed; }
        .btn-test { background: #2196F3; }
        .status { margin: 20px 0; padding: 15px; border-radius: 4px; }
        .status.success { background: #e8f5e9; color: #2e7d32; }
        .status.error { background: #ffebee; color: #c62828; }
        .step { margin: 20px 0; padding: 15px; background: #fafafa; border-radius: 4px; }
        .step-num { display: inline-block; width: 30px; height: 30px; background: #4CAF50; color: white; border-radius: 50%; text-align: center; line-height: 30px; margin-right: 10px; }
    </style>
</head>
<body>
    <h1>📱 Gmail Approval Daemon Setup</h1>

    <div class="step">
        <span class="step-num">1</span>
        <strong>Install the ntfy app</strong>
        <p>Download from <a href="https://ntfy.sh" target="_blank">ntfy.sh</a> or your app store.</p>
    </div>

    <div class="step">
        <span class="step-num">2</span>
        <strong>Subscribe to your private topic</strong>
        <p>Scan this QR code with the ntfy app, or manually subscribe to:</p>
        <div class="qr-container">
            <img src="data:image/png;base64,{{.QRCode}}" alt="QR Code">
        </div>
        <div class="topic">{{.Topic}}</div>
    </div>

    <div class="step">
        <span class="step-num">3</span>
        <strong>Test the connection</strong>
        <button class="btn btn-test" onclick="testNotification()">Send Test Notification</button>
        <div id="status"></div>
    </div>

    <div class="step">
        <span class="step-num">4</span>
        <strong>Complete setup</strong>
        <button class="btn" id="complete-btn" onclick="completeSetup()" disabled>Complete Setup</button>
        <p><small>Button enables after successful test</small></p>
    </div>

    <script>
        let testSuccessful = false;

        async function testNotification() {
            const status = document.getElementById('status');
            status.className = 'status';
            status.textContent = 'Sending test notification...';

            try {
                const resp = await fetch('/test', { method: 'POST' });
                const data = await resp.json();
                if (data.success) {
                    status.className = 'status success';
                    status.textContent = '✓ Test notification sent! Check your phone.';
                    testSuccessful = true;
                    document.getElementById('complete-btn').disabled = false;
                } else {
                    status.className = 'status error';
                    status.textContent = '✗ Failed: ' + data.error;
                }
            } catch (err) {
                status.className = 'status error';
                status.textContent = '✗ Error: ' + err.message;
            }
        }

        async function completeSetup() {
            if (!testSuccessful) return;

            try {
                const resp = await fetch('/complete', { method: 'POST' });
                const data = await resp.json();
                if (data.success) {
                    document.body.innerHTML = '<h1>✓ Setup Complete!</h1><p>You can close this window. The daemon is now running.</p>';
                }
            } catch (err) {
                alert('Error completing setup: ' + err.message);
            }
        }
    </script>
</body>
</html>`
```

**Step 2: Update main.go to use setup server**

Modify `cmd/approval-daemon/main.go`, replace the `run()` function:

```go
func run() error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// First run or reset - need setup
	if config == nil || !config.SetupComplete {
		if config == nil {
			config, err = createNewConfig()
			if err != nil {
				return fmt.Errorf("failed to create config: %w", err)
			}
			if err := saveConfig(config); err != nil {
				return fmt.Errorf("failed to save initial config: %w", err)
			}
		}

		setupServer, err := newSetupServer(config)
		if err != nil {
			return fmt.Errorf("failed to create setup server: %w", err)
		}
		if err := setupServer.run(); err != nil {
			return fmt.Errorf("setup failed: %w", err)
		}

		// Reload config after setup
		config, err = loadConfig()
		if err != nil {
			return fmt.Errorf("failed to reload config: %w", err)
		}
	}

	log.Println("Setup complete. Daemon ready.")
	log.Printf("ntfy topic: %s", config.NtfyTopic)

	// TODO: Start normal operation
	select {} // Block forever for now
}
```

**Step 3: Verify it builds and runs**

```bash
go build -o gmail-approval-daemon ./cmd/approval-daemon
./gmail-approval-daemon
```

Expected: Opens browser with setup page

**Step 4: Commit**

```bash
git add cmd/approval-daemon/setup.go cmd/approval-daemon/main.go
git commit -m "feat: add setup web server with QR code

- First run shows setup page with QR code for ntfy subscription
- Test notification button verifies connection
- Must complete test before finishing setup
- Config saved after successful setup
"
```

---

## Task 4: Implement ntfy.sh Client

**Files:**
- Create: `cmd/approval-daemon/ntfy.go`

**Step 1: Create ntfy client**

Create `cmd/approval-daemon/ntfy.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const ntfyBaseURL = "https://ntfy.sh"

type NtfyAction struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	URL    string `json:"url,omitempty"`
	Method string `json:"method,omitempty"`
	Body   string `json:"body,omitempty"`
}

type NtfyMessage struct {
	Topic    string       `json:"topic"`
	Title    string       `json:"title,omitempty"`
	Message  string       `json:"message"`
	Priority int          `json:"priority,omitempty"`
	Tags     []string     `json:"tags,omitempty"`
	Actions  []NtfyAction `json:"actions,omitempty"`
}

type NtfyPollMessage struct {
	ID      string `json:"id"`
	Time    int64  `json:"time"`
	Event   string `json:"event"`
	Topic   string `json:"topic"`
	Message string `json:"message"`
}

func sendNtfyNotification(topic, title, message string) error {
	msg := NtfyMessage{
		Topic:   topic,
		Title:   title,
		Message: message,
	}
	return sendNtfyMessage(msg)
}

func sendNtfyMessageWithActions(topic, title, message string, actions []NtfyAction) error {
	msg := NtfyMessage{
		Topic:    topic,
		Title:    title,
		Message:  message,
		Priority: 4, // High priority for approval requests
		Tags:     []string{"email", "outgoing_envelope"},
		Actions:  actions,
	}
	return sendNtfyMessage(msg)
}

func sendNtfyMessage(msg NtfyMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	resp, err := http.Post(ntfyBaseURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to send notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ntfy returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func pollNtfyMessages(topic string, since time.Time) ([]NtfyPollMessage, error) {
	pollURL := fmt.Sprintf("%s/%s/json?poll=1&since=%d", ntfyBaseURL, url.PathEscape(topic), since.Unix())

	resp, err := http.Get(pollURL)
	if err != nil {
		return nil, fmt.Errorf("failed to poll: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ntfy poll returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read poll response: %w", err)
	}

	// ntfy returns newline-delimited JSON
	var messages []NtfyPollMessage
	for _, line := range bytes.Split(body, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var msg NtfyPollMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // Skip malformed messages
		}
		if msg.Event == "message" {
			messages = append(messages, msg)
		}
	}
	return messages, nil
}
```

**Step 2: Verify it compiles**

```bash
go build -o gmail-approval-daemon ./cmd/approval-daemon
```

Expected: No errors

**Step 3: Commit**

```bash
git add cmd/approval-daemon/ntfy.go
git commit -m "feat: add ntfy.sh client for notifications and polling

- sendNtfyNotification for simple messages
- sendNtfyMessageWithActions for approval buttons
- pollNtfyMessages for checking responses
"
```

---

## Task 5: Implement Unix Socket IPC Server

**Files:**
- Create: `cmd/approval-daemon/socket.go`

**Step 1: Create socket server**

Create `cmd/approval-daemon/socket.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
)

type SocketServer struct {
	listener net.Listener
	daemon   *ApprovalDaemon
}

type IPCRequest struct {
	Action  string `json:"action"`
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
	DraftID string `json:"draft_id,omitempty"`
}

type IPCResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	Status  string `json:"status,omitempty"`
}

func getSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gmail-mcp", "approval.sock")
}

func newSocketServer(daemon *ApprovalDaemon) (*SocketServer, error) {
	socketPath := getSocketPath()

	// Ensure directory exists
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create socket dir: %w", err)
	}

	// Remove existing socket
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create socket: %w", err)
	}

	// Set socket permissions
	if err := os.Chmod(socketPath, 0600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("failed to set socket permissions: %w", err)
	}

	return &SocketServer{
		listener: listener,
		daemon:   daemon,
	}, nil
}

func (s *SocketServer) run() {
	log.Printf("Socket server listening on %s", getSocketPath())
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			log.Printf("Socket accept error: %v", err)
			continue
		}
		go s.handleConnection(conn)
	}
}

func (s *SocketServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	var req IPCRequest
	if err := decoder.Decode(&req); err != nil {
		encoder.Encode(IPCResponse{Success: false, Error: "invalid request"})
		return
	}

	switch req.Action {
	case "queue_email":
		resp := s.daemon.queueEmail(req)
		encoder.Encode(resp)
	case "status":
		encoder.Encode(IPCResponse{Success: true, Status: "running"})
	default:
		encoder.Encode(IPCResponse{Success: false, Error: "unknown action"})
	}
}

func (s *SocketServer) close() {
	s.listener.Close()
	os.Remove(getSocketPath())
}
```

**Step 2: Verify it compiles**

```bash
go build -o gmail-approval-daemon ./cmd/approval-daemon
```

Expected: Error about ApprovalDaemon not defined (expected, we'll create it next)

**Step 3: Commit**

```bash
git add cmd/approval-daemon/socket.go
git commit -m "feat: add Unix socket IPC server

- Listens on ~/.config/gmail-mcp/approval.sock
- Handles queue_email and status actions
- Secure permissions (0600) on socket file
"
```

---

## Task 6: Implement Approval Daemon Core Logic

**Files:**
- Create: `cmd/approval-daemon/daemon.go`
- Modify: `cmd/approval-daemon/main.go`

**Step 1: Create daemon with approval logic**

Create `cmd/approval-daemon/daemon.go`:

```go
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type PendingEmail struct {
	DraftID      string
	To           string
	Subject      string
	Body         string
	ApproveToken string
	RejectToken  string
	QueuedAt     time.Time
	ResultChan   chan ApprovalResult
}

type ApprovalResult struct {
	Approved bool
	Error    error
}

type ApprovalDaemon struct {
	config  *Config
	pending *PendingEmail
	mu      sync.Mutex
}

func newApprovalDaemon(config *Config) *ApprovalDaemon {
	return &ApprovalDaemon{
		config: config,
	}
}

func (d *ApprovalDaemon) queueEmail(req IPCRequest) IPCResponse {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.pending != nil {
		return IPCResponse{
			Success: false,
			Error:   "another email is pending approval - only one at a time",
		}
	}

	// Generate one-time tokens
	approveToken, _ := generateToken()
	rejectToken, _ := generateToken()

	d.pending = &PendingEmail{
		DraftID:      req.DraftID,
		To:           req.To,
		Subject:      req.Subject,
		Body:         req.Body,
		ApproveToken: approveToken,
		RejectToken:  rejectToken,
		QueuedAt:     time.Now(),
		ResultChan:   make(chan ApprovalResult, 1),
	}

	// Send notification
	if err := d.sendApprovalNotification(); err != nil {
		d.pending = nil
		return IPCResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to send notification: %v", err),
		}
	}

	log.Printf("📧 Email queued for approval: to=%s subject=%s", req.To, req.Subject)

	// Wait for approval (blocking)
	select {
	case result := <-d.pending.ResultChan:
		d.pending = nil
		if result.Error != nil {
			return IPCResponse{Success: false, Error: result.Error.Error()}
		}
		if result.Approved {
			return IPCResponse{Success: true, Status: "approved"}
		}
		return IPCResponse{Success: false, Error: "rejected by user"}
	case <-time.After(5 * time.Minute):
		d.pending = nil
		return IPCResponse{Success: false, Error: "approval timed out"}
	}
}

func (d *ApprovalDaemon) sendApprovalNotification() error {
	truncatedBody := d.pending.Body
	if len(truncatedBody) > 200 {
		truncatedBody = truncatedBody[:200] + "..."
	}

	message := fmt.Sprintf("To: %s\nSubject: %s\n\n%s",
		d.pending.To, d.pending.Subject, truncatedBody)

	actions := []NtfyAction{
		{
			Action: "http",
			Label:  "✓ Approve",
			URL:    fmt.Sprintf("%s/%s", ntfyBaseURL, d.config.NtfyTopic),
			Method: "POST",
			Body:   "APPROVE:" + d.pending.ApproveToken,
		},
		{
			Action: "http",
			Label:  "✗ Reject",
			URL:    fmt.Sprintf("%s/%s", ntfyBaseURL, d.config.NtfyTopic),
			Method: "POST",
			Body:   "REJECT:" + d.pending.RejectToken,
		},
	}

	return sendNtfyMessageWithActions(d.config.NtfyTopic, "📧 Approve email?", message, actions)
}

func (d *ApprovalDaemon) startPolling() {
	since := time.Now()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		d.mu.Lock()
		pending := d.pending
		d.mu.Unlock()

		if pending == nil {
			continue
		}

		messages, err := pollNtfyMessages(d.config.NtfyTopic, since)
		if err != nil {
			log.Printf("Poll error: %v", err)
			continue
		}

		for _, msg := range messages {
			d.handlePollMessage(msg, pending)
		}
	}
}

func (d *ApprovalDaemon) handlePollMessage(msg NtfyPollMessage, pending *PendingEmail) {
	if strings.HasPrefix(msg.Message, "APPROVE:") {
		token := strings.TrimPrefix(msg.Message, "APPROVE:")
		if token == pending.ApproveToken {
			log.Println("✅ Email approved by user")
			pending.ResultChan <- ApprovalResult{Approved: true}
		}
	} else if strings.HasPrefix(msg.Message, "REJECT:") {
		token := strings.TrimPrefix(msg.Message, "REJECT:")
		if token == pending.RejectToken {
			log.Println("❌ Email rejected by user")
			pending.ResultChan <- ApprovalResult{Approved: false}
		}
	}
}

func generateToken() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
```

**Step 2: Update main.go to run daemon**

Replace the end of the `run()` function in `cmd/approval-daemon/main.go`:

```go
func run() error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// First run or reset - need setup
	if config == nil || !config.SetupComplete {
		if config == nil {
			config, err = createNewConfig()
			if err != nil {
				return fmt.Errorf("failed to create config: %w", err)
			}
			if err := saveConfig(config); err != nil {
				return fmt.Errorf("failed to save initial config: %w", err)
			}
		}

		setupServer, err := newSetupServer(config)
		if err != nil {
			return fmt.Errorf("failed to create setup server: %w", err)
		}
		if err := setupServer.run(); err != nil {
			return fmt.Errorf("setup failed: %w", err)
		}

		// Reload config after setup
		config, err = loadConfig()
		if err != nil {
			return fmt.Errorf("failed to reload config: %w", err)
		}
	}

	// Create and start daemon
	daemon := newApprovalDaemon(config)

	// Start socket server
	socketServer, err := newSocketServer(daemon)
	if err != nil {
		return fmt.Errorf("failed to create socket server: %w", err)
	}
	defer socketServer.close()

	// Start polling in background
	go daemon.startPolling()

	log.Println("═══════════════════════════════════════════════════════════════")
	log.Println("📱 APPROVAL DAEMON RUNNING")
	log.Println("═══════════════════════════════════════════════════════════════")
	log.Printf("   Socket: %s", getSocketPath())
	log.Printf("   ntfy topic: %s", config.NtfyTopic)
	log.Println("   Waiting for email approval requests...")
	log.Println("═══════════════════════════════════════════════════════════════")

	// Run socket server (blocking)
	socketServer.run()
	return nil
}
```

**Step 3: Verify it builds**

```bash
go build -o gmail-approval-daemon ./cmd/approval-daemon
```

Expected: No errors

**Step 4: Commit**

```bash
git add cmd/approval-daemon/daemon.go cmd/approval-daemon/main.go
git commit -m "feat: implement approval daemon core logic

- Queue emails with one-time approve/reject tokens
- Send ntfy notifications with action buttons
- Poll for responses and validate tokens
- 5 minute timeout on pending approvals
"
```

---

## Task 7: Modify MCP Server to Use Daemon

**Files:**
- Modify: `main.go` (in project root)

**Step 1: Add socket client function**

Add to `main.go` near the top with other functions:

```go
func sendToDaemon(req map[string]string) (map[string]interface{}, error) {
	home, _ := os.UserHomeDir()
	socketPath := filepath.Join(home, ".config", "gmail-mcp", "approval.sock")

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("approval daemon not running. Start it with: gmail-approval-daemon")
	}
	defer conn.Close()

	// Set deadline for the entire operation
	conn.SetDeadline(time.Now().Add(6 * time.Minute)) // 5 min timeout + buffer

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send request to daemon: %w", err)
	}

	var resp map[string]interface{}
	if err := decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to read daemon response: %w", err)
	}

	return resp, nil
}
```

**Step 2: Modify send_email_ato tool handler**

Find the `send_email_ato` tool handler and replace it to use the daemon:

```go
// In the tool handler for send_email_ato, replace the approval logic with:

// Send to approval daemon instead of local OOB
resp, err := sendToDaemon(map[string]string{
	"action":   "queue_email",
	"to":       to,
	"subject":  subject,
	"body":     body,
	"draft_id": draftID,
})
if err != nil {
	return mcp.NewToolResultError(err.Error()), nil
}

success, _ := resp["success"].(bool)
if !success {
	errMsg, _ := resp["error"].(string)
	return mcp.NewToolResultError(errMsg), nil
}

// Approved - send the draft
err = gmailServer.SendDraft(draftID)
if err != nil {
	return mcp.NewToolResultError(fmt.Sprintf("approved but failed to send: %v", err)), nil
}

resultJSON, _ := json.MarshalIndent(map[string]interface{}{
	"status":  "sent",
	"message": "Email approved and sent successfully",
	"to":      to,
	"subject": subject,
}, "", "  ")
return mcp.NewToolResultText(string(resultJSON)), nil
```

**Step 3: Add required imports**

Add to imports in `main.go`:

```go
import (
	// ... existing imports ...
	"path/filepath"
)
```

**Step 4: Remove old TOTP code**

Remove the TOTP-related code from `main.go`:
- Remove `github.com/pquerna/otp` and `github.com/pquerna/otp/totp` imports
- Remove `TOTPKey` field from `ApprovalSession`
- Remove TOTP generation in `NewApprovalSession`
- Remove TOTP validation in approval endpoint
- Remove TOTP UI elements from dashboard HTML
- Remove TOTP startup log messages

**Step 5: Verify it builds**

```bash
go build -o gmail-mcp-server .
```

Expected: No errors

**Step 6: Commit**

```bash
git add main.go go.mod go.sum
git commit -m "feat: integrate MCP server with approval daemon

- Add socket client to communicate with daemon
- send_email_ato now sends to daemon for approval
- Remove TOTP code (replaced by mobile push in daemon)
"
```

---

## Task 8: Update Documentation

**Files:**
- Modify: `README.md`
- Modify: `docs/plans/2026-01-16-mobile-push-approval-design.md`

**Step 1: Update README with new setup instructions**

Add section to README.md:

```markdown
## Secure Email Sending (Mobile Push Approval)

This server uses a separate approval daemon for secure email sending. The agent cannot bypass this - approval happens on your phone.

### Setup

1. **Build both binaries:**
   ```bash
   go build -o gmail-mcp-server .
   go build -o gmail-approval-daemon ./cmd/approval-daemon
   ```

2. **Start the approval daemon (first time):**
   ```bash
   ./gmail-approval-daemon
   ```
   - Opens browser with setup page
   - Install [ntfy app](https://ntfy.sh) on your phone
   - Scan QR code to subscribe to your private topic
   - Click "Send Test Notification" and verify you receive it
   - Click "Complete Setup"

3. **Start the MCP server:**
   ```bash
   ./gmail-mcp-server
   ```

### Daily Use

1. Start the approval daemon: `./gmail-approval-daemon`
2. Start the MCP server (in your agent config)
3. When agent sends email, you get a push notification
4. Tap Approve or Reject on your phone

### Resetting

To regenerate your ntfy topic (new phone, etc.):
```bash
./gmail-approval-daemon --reset
```
```

**Step 2: Commit documentation**

```bash
git add README.md
git commit -m "docs: update README with mobile push approval setup

- Add setup instructions for approval daemon
- Document daily usage workflow
- Add reset instructions
"
```

---

## Task 9: Integration Testing

**Step 1: Build both binaries**

```bash
go build -o gmail-mcp-server .
go build -o gmail-approval-daemon ./cmd/approval-daemon
```

**Step 2: Test daemon setup flow**

```bash
./gmail-approval-daemon --reset
./gmail-approval-daemon
```

- Verify browser opens with setup page
- Verify QR code displays
- Subscribe in ntfy app
- Test notification works
- Complete setup

**Step 3: Test approval flow manually**

In one terminal:
```bash
./gmail-approval-daemon
```

In another terminal, test socket communication:
```bash
echo '{"action":"status"}' | nc -U ~/.config/gmail-mcp/approval.sock
```

Expected: `{"success":true,"status":"running"}`

**Step 4: Test full flow with MCP server**

Start both binaries and use the agent to send a test email. Verify:
- Notification arrives on phone
- Approve button sends email
- Reject button blocks email

**Step 5: Commit any fixes**

```bash
git add -A
git commit -m "fix: integration testing fixes"
```

---

## Summary

After completing all tasks, you will have:

1. **gmail-approval-daemon** binary with:
   - First-run setup with QR code
   - ntfy.sh integration for push notifications
   - Unix socket IPC server
   - Polling for approve/reject responses
   - One-time token validation

2. **gmail-mcp-server** modified to:
   - Send approval requests to daemon via socket
   - Wait for daemon response (blocking)
   - Send email only after approval

3. **Security properties:**
   - Agent cannot access daemon's secrets
   - Agent cannot forge approval tokens
   - Approval requires physical phone interaction
