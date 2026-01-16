package main

import (
	"context"
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
	// Use HTTPS URL so iOS Camera recognizes it and opens Safari -> ntfy app
	ntfyURL := fmt.Sprintf("https://ntfy.sh/%s", s.config.NtfyTopic)
	qr, err := qrcode.Encode(ntfyURL, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "Failed to generate QR code", 500)
		return
	}
	qrBase64 := base64.StdEncoding.EncodeToString(qr)

	tmpl := template.Must(template.New("setup").Parse(setupHTML))
	tmpl.Execute(w, map[string]string{
		"Topic":  s.config.NtfyTopic,
		"QRCode": qrBase64,
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
        <p>Scan this QR code with your phone's camera. It will open ntfy.sh where you can subscribe.</p>
        <div class="qr-container">
            <img src="data:image/png;base64,{{.QRCode}}" alt="QR Code">
        </div>
        <p style="margin-top: 10px; font-size: 14px; color: #666;">Or manually subscribe to this topic in the ntfy app:</p>
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
