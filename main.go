package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/joho/godotenv"
	"github.com/ledongthuc/pdf"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/nguyenthenguyen/docx"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	googleOption "google.golang.org/api/option"

	"net"
)

type GmailServer struct {
	service *gmail.Service
	userID  string
}

// ============================================================================
// OOB Approval System - Agent Cut-Out Pattern
// See docs/agent-cut-out-pattern.md for details
// ============================================================================

// ApprovalResult is sent back when approval completes (used by OOB dashboard)
type ApprovalResult struct {
	Approved bool
	Error    error
}

// PendingEmail represents an email waiting for user approval
type PendingEmail struct {
	ID       string              // Unique ID for this pending request
	DraftID  string              // Gmail draft ID
	To       string              // Recipient
	Subject  string              // Email subject
	Body     string              // Full email body
	QueuedAt time.Time           // When the request was queued
	ResultCh chan ApprovalResult // Channel to send result back to blocked caller
}

// ApprovalSession manages the OOB approval state
type ApprovalSession struct {
	ID         string // Crypto-random session ID for URL
	CreatedAt  time.Time
	Pending    *PendingEmail // Only ONE pending email at a time
	History    []EmailHistoryEntry
	mu         sync.Mutex
	sseClients map[chan string]bool // SSE clients for real-time updates
}

// EmailHistoryEntry records sent/rejected emails
type EmailHistoryEntry struct {
	DraftID   string
	To        string
	Subject   string
	Action    string // "sent" or "rejected"
	Timestamp time.Time
}

// Global approval session (created on server start)
var approvalSession *ApprovalSession

// NewApprovalSession creates a new session with a crypto-random ID
func NewApprovalSession() (*ApprovalSession, error) {
	// Generate 32 bytes of randomness for session ID
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return nil, fmt.Errorf("failed to generate session ID: %v", err)
	}

	sessionID := base64.URLEncoding.EncodeToString(randomBytes)
	// Remove padding for cleaner URLs
	sessionID = strings.TrimRight(sessionID, "=")

	return &ApprovalSession{
		ID:         sessionID,
		CreatedAt:  time.Now(),
		History:    make([]EmailHistoryEntry, 0),
		sseClients: make(map[chan string]bool),
	}, nil
}

// sendToDaemon sends a request to the approval daemon via Unix socket
func sendToDaemon(req map[string]string) (map[string]interface{}, error) {
	home, _ := os.UserHomeDir()
	socketPath := filepath.Join(home, ".config", "gmail-mcp", "approval.sock")

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("approval daemon not running. Start it with: gmail-approval-daemon")
	}
	defer conn.Close()

	// Set deadline for the entire operation (5 min approval timeout + buffer)
	conn.SetDeadline(time.Now().Add(6 * time.Minute))

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

// QueueEmail queues an email for approval, returns error if one is already pending
func (s *ApprovalSession) QueueEmail(draftID, to, subject, body string) (*PendingEmail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Pending != nil {
		return nil, fmt.Errorf("another email is already pending approval - only one at a time allowed")
	}

	// Generate unique ID for this pending request
	idBytes := make([]byte, 8)
	rand.Read(idBytes)
	pendingID := base64.URLEncoding.EncodeToString(idBytes)
	pendingID = strings.TrimRight(pendingID, "=")

	pending := &PendingEmail{
		ID:       pendingID,
		DraftID:  draftID,
		To:       to,
		Subject:  subject,
		Body:     body,
		QueuedAt: time.Now(),
		ResultCh: make(chan ApprovalResult, 1),
	}

	s.Pending = pending

	// Notify SSE clients
	s.broadcastUpdate()

	return pending, nil
}

// Approve approves the pending email
func (s *ApprovalSession) Approve() (*PendingEmail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Pending == nil {
		return nil, fmt.Errorf("no email pending approval")
	}

	pending := s.Pending
	s.Pending = nil

	// Record in history
	s.History = append(s.History, EmailHistoryEntry{
		DraftID:   pending.DraftID,
		To:        pending.To,
		Subject:   pending.Subject,
		Action:    "sent",
		Timestamp: time.Now(),
	})

	// Notify SSE clients
	s.broadcastUpdate()

	return pending, nil
}

// Reject rejects the pending email
func (s *ApprovalSession) Reject() (*PendingEmail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Pending == nil {
		return nil, fmt.Errorf("no email pending approval")
	}

	pending := s.Pending
	s.Pending = nil

	// Record in history
	s.History = append(s.History, EmailHistoryEntry{
		DraftID:   pending.DraftID,
		To:        pending.To,
		Subject:   pending.Subject,
		Action:    "rejected",
		Timestamp: time.Now(),
	})

	// Notify SSE clients
	s.broadcastUpdate()

	return pending, nil
}

// GetPending returns the current pending email (thread-safe)
func (s *ApprovalSession) GetPending() *PendingEmail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Pending
}

// AddSSEClient registers a new SSE client
func (s *ApprovalSession) AddSSEClient(ch chan string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sseClients[ch] = true
}

// RemoveSSEClient unregisters an SSE client
func (s *ApprovalSession) RemoveSSEClient(ch chan string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sseClients, ch)
	close(ch)
}

// broadcastUpdate notifies all SSE clients of a state change
func (s *ApprovalSession) broadcastUpdate() {
	for ch := range s.sseClients {
		select {
		case ch <- "update":
		default:
			// Client not ready, skip
		}
	}
}

// ============================================================================
// OOB Web Server - Agent-Inaccessible Approval Dashboard
// ============================================================================

const oobServerPort = 8787

// StartOOBServer starts the out-of-band approval web server
func StartOOBServer(gmailServer *GmailServer) {
	mux := http.NewServeMux()

	// Dashboard page
	mux.HandleFunc("/outbox/", func(w http.ResponseWriter, r *http.Request) {
		// Extract session ID from path
		pathParts := strings.Split(r.URL.Path, "/")
		if len(pathParts) < 3 || pathParts[2] == "" {
			http.Error(w, "Invalid session URL", http.StatusBadRequest)
			return
		}
		sessionID := pathParts[2]

		// Validate session ID
		if approvalSession == nil || approvalSession.ID != sessionID {
			http.Error(w, "Invalid or expired session", http.StatusForbidden)
			return
		}

		// Serve the dashboard HTML
		serveDashboard(w, r)
	})

	// API: Get pending email
	mux.HandleFunc("/api/pending/", func(w http.ResponseWriter, r *http.Request) {
		pathParts := strings.Split(r.URL.Path, "/")
		if len(pathParts) < 4 {
			http.Error(w, "Invalid URL", http.StatusBadRequest)
			return
		}
		sessionID := pathParts[3]

		if approvalSession == nil || approvalSession.ID != sessionID {
			http.Error(w, "Invalid session", http.StatusForbidden)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		pending := approvalSession.GetPending()
		if pending == nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"pending": false,
			})
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"pending":   true,
			"id":        pending.ID,
			"draftId":   pending.DraftID,
			"to":        pending.To,
			"subject":   pending.Subject,
			"body":      pending.Body,
			"queuedAt":  pending.QueuedAt.Format(time.RFC3339),
			"expiresIn": int(5*time.Minute - time.Since(pending.QueuedAt).Round(time.Second)/time.Second),
		})
	})

	// API: Approve pending email
	mux.HandleFunc("/api/approve/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		pathParts := strings.Split(r.URL.Path, "/")
		if len(pathParts) < 4 {
			http.Error(w, "Invalid URL", http.StatusBadRequest)
			return
		}
		sessionID := pathParts[3]

		if approvalSession == nil || approvalSession.ID != sessionID {
			http.Error(w, "Invalid session", http.StatusForbidden)
			return
		}

		log.Printf("✅ Email approved via dashboard")

		pending, err := approvalSession.Approve()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Send the email via Gmail API
		err = gmailServer.SendDraft(pending.DraftID)
		if err != nil {
			// Put back in history as failed
			log.Printf("Failed to send email: %v", err)
			pending.ResultCh <- ApprovalResult{Approved: false, Error: err}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   err.Error(),
			})
			return
		}

		// Notify the blocked caller
		pending.ResultCh <- ApprovalResult{Approved: true, Error: nil}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Email sent successfully",
		})
	})

	// API: Reject pending email
	mux.HandleFunc("/api/reject/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		pathParts := strings.Split(r.URL.Path, "/")
		if len(pathParts) < 4 {
			http.Error(w, "Invalid URL", http.StatusBadRequest)
			return
		}
		sessionID := pathParts[3]

		if approvalSession == nil || approvalSession.ID != sessionID {
			http.Error(w, "Invalid session", http.StatusForbidden)
			return
		}

		pending, err := approvalSession.Reject()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Notify the blocked caller
		pending.ResultCh <- ApprovalResult{Approved: false, Error: nil}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Email rejected",
		})
	})

	// SSE: Real-time updates
	mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
		pathParts := strings.Split(r.URL.Path, "/")
		if len(pathParts) < 3 {
			http.Error(w, "Invalid URL", http.StatusBadRequest)
			return
		}
		sessionID := pathParts[2]

		if approvalSession == nil || approvalSession.ID != sessionID {
			http.Error(w, "Invalid session", http.StatusForbidden)
			return
		}

		// Set SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Create channel for this client
		clientCh := make(chan string, 10)
		approvalSession.AddSSEClient(clientCh)
		defer approvalSession.RemoveSSEClient(clientCh)

		// Get the flusher
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "SSE not supported", http.StatusInternalServerError)
			return
		}

		// Send initial event
		fmt.Fprintf(w, "data: connected\n\n")
		flusher.Flush()

		// Listen for updates or client disconnect
		for {
			select {
			case msg := <-clientCh:
				fmt.Fprintf(w, "data: %s\n\n", msg)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	// Start server in goroutine
	go func() {
		addr := fmt.Sprintf(":%d", oobServerPort)
		log.Printf("🌐 OOB Approval server starting on http://localhost%s", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("OOB Server error: %v", err)
		}
	}()
}

// serveDashboard renders the approval dashboard HTML
func serveDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	tmpl := template.Must(template.New("dashboard").Parse(dashboardHTML))
	tmpl.Execute(w, map[string]interface{}{
		"SessionID": approvalSession.ID,
		"Port":      oobServerPort,
	})
}

// SendDraft sends a draft via Gmail API
func (g *GmailServer) SendDraft(draftID string) error {
	_, err := g.service.Users.Drafts.Send(g.userID, &gmail.Draft{Id: draftID}).Do()
	if err != nil {
		return fmt.Errorf("failed to send draft: %v", err)
	}
	return nil
}

// Dashboard HTML template
const dashboardHTML = `<!DOCTYPE html>
<html>
<head>
    <title>Gmail Outbox - Agent-Safe Review</title>
    <meta charset="utf-8">
    <style>
        * { box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
            max-width: 800px;
            margin: 0 auto;
            padding: 20px;
            background: #f5f5f5;
        }
        h1 {
            color: #333;
            border-bottom: 2px solid #4CAF50;
            padding-bottom: 10px;
        }
        .status {
            padding: 15px;
            border-radius: 8px;
            margin-bottom: 20px;
        }
        .status.waiting {
            background: #e3f2fd;
            border: 1px solid #90caf9;
            color: #1565c0;
        }
        .status.pending {
            background: #fff3e0;
            border: 1px solid #ffcc80;
            color: #e65100;
        }
        .email-card {
            background: white;
            border-radius: 8px;
            padding: 20px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
            margin-bottom: 20px;
        }
        .email-header {
            border-bottom: 1px solid #eee;
            padding-bottom: 15px;
            margin-bottom: 15px;
        }
        .email-field {
            margin-bottom: 8px;
        }
        .email-field label {
            font-weight: 600;
            color: #666;
            display: inline-block;
            width: 80px;
        }
        .email-body {
            background: #fafafa;
            border: 1px solid #eee;
            border-radius: 4px;
            padding: 15px;
            white-space: pre-wrap;
            font-family: inherit;
            line-height: 1.5;
            max-height: 400px;
            overflow-y: auto;
        }
        .buttons {
            display: flex;
            gap: 15px;
            margin-top: 20px;
        }
        button {
            flex: 1;
            padding: 15px 30px;
            font-size: 16px;
            font-weight: 600;
            border: none;
            border-radius: 8px;
            cursor: pointer;
            transition: transform 0.1s, box-shadow 0.1s;
        }
        button:hover {
            transform: translateY(-1px);
            box-shadow: 0 4px 8px rgba(0,0,0,0.2);
        }
        button:active {
            transform: translateY(0);
        }
        .btn-approve {
            background: #4CAF50;
            color: white;
        }
        .btn-reject {
            background: #f44336;
            color: white;
        }
        button:disabled {
            opacity: 0.5;
            cursor: not-allowed;
            transform: none;
            box-shadow: none;
        }
        .history {
            margin-top: 30px;
        }
        .history h2 {
            color: #666;
            font-size: 14px;
            text-transform: uppercase;
        }
        .history-item {
            padding: 10px;
            border-bottom: 1px solid #eee;
            font-size: 14px;
        }
        .history-item.sent { border-left: 3px solid #4CAF50; }
        .history-item.rejected { border-left: 3px solid #f44336; }
        .footer {
            text-align: center;
            color: #999;
            font-size: 12px;
            margin-top: 30px;
        }
        .pulse {
            animation: pulse 2s infinite;
        }
        @keyframes pulse {
            0%, 100% { opacity: 1; }
            50% { opacity: 0.5; }
        }
    </style>
</head>
<body>
    <h1>📤 Gmail Outbox</h1>
    <p style="color: #666;">Agent-Safe Review Dashboard</p>

    <div id="status" class="status waiting">
        <span class="pulse">⏳</span> Waiting for emails to review...
    </div>

    <div id="email-container" style="display: none;">
        <div class="email-card">
            <div class="email-header">
                <div class="email-field">
                    <label>To:</label>
                    <span id="email-to"></span>
                </div>
                <div class="email-field">
                    <label>Subject:</label>
                    <span id="email-subject"></span>
                </div>
            </div>
            <div class="email-body" id="email-body"></div>
            <div class="buttons">
                <button class="btn-approve" onclick="approve()" id="btn-approve">
                    ✓ APPROVE & SEND
                </button>
                <button class="btn-reject" onclick="reject()" id="btn-reject">
                    ✗ REJECT
                </button>
            </div>
        </div>
    </div>

    <div class="history" id="history-container" style="display: none;">
        <h2>History</h2>
        <div id="history-list"></div>
    </div>

    <div class="footer">
        Session: {{.SessionID}}<br>
        This dashboard is agent-inaccessible. Only you can approve emails.
    </div>

    <script>
        const sessionID = "{{.SessionID}}";
        const port = {{.Port}};
        let currentPendingId = null;

        // Connect to SSE for real-time updates
        const evtSource = new EventSource("/events/" + sessionID);
        evtSource.onmessage = function(event) {
            console.log("SSE event:", event.data);
            fetchPending();
        };
        evtSource.onerror = function(err) {
            console.error("SSE error:", err);
            // Fall back to polling
            setInterval(fetchPending, 2000);
        };

        async function fetchPending() {
            try {
                const resp = await fetch("/api/pending/" + sessionID);
                const data = await resp.json();

                if (data.pending) {
                    currentPendingId = data.id;
                    document.getElementById("status").className = "status pending";
                    document.getElementById("status").innerHTML =
                        "<strong>⚠️ Email pending approval</strong>";
                    document.getElementById("email-to").textContent = data.to;
                    document.getElementById("email-subject").textContent = data.subject;
                    document.getElementById("email-body").textContent = data.body;
                    document.getElementById("email-container").style.display = "block";
                    document.getElementById("btn-approve").disabled = false;
                    document.getElementById("btn-reject").disabled = false;
                } else {
                    currentPendingId = null;
                    document.getElementById("status").className = "status waiting";
                    document.getElementById("status").innerHTML =
                        '<span class="pulse">⏳</span> Waiting for emails to review...';
                    document.getElementById("email-container").style.display = "none";
                }
            } catch (err) {
                console.error("Error fetching pending:", err);
            }
        }

        async function approve() {
            if (!currentPendingId) return;

            document.getElementById("btn-approve").disabled = true;
            document.getElementById("btn-reject").disabled = true;
            document.getElementById("btn-approve").textContent = "Sending...";

            try {
                const resp = await fetch("/api/approve/" + sessionID, {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({})
                });
                const data = await resp.json();

                if (data.success) {
                    document.getElementById("btn-approve").textContent = "✓ Sent!";
                    addToHistory("sent",
                        document.getElementById("email-to").textContent,
                        document.getElementById("email-subject").textContent);
                    setTimeout(fetchPending, 1000);
                } else {
                    alert("Failed to send: " + data.error);
                    document.getElementById("btn-approve").textContent = "✓ APPROVE & SEND";
                    document.getElementById("btn-approve").disabled = false;
                    document.getElementById("btn-reject").disabled = false;
                }
            } catch (err) {
                alert("Error: " + err);
                document.getElementById("btn-approve").textContent = "✓ APPROVE & SEND";
                document.getElementById("btn-approve").disabled = false;
                document.getElementById("btn-reject").disabled = false;
            }
        }

        async function reject() {
            if (!currentPendingId) return;

            document.getElementById("btn-approve").disabled = true;
            document.getElementById("btn-reject").disabled = true;

            try {
                const resp = await fetch("/api/reject/" + sessionID, { method: "POST" });
                const data = await resp.json();

                if (data.success) {
                    addToHistory("rejected",
                        document.getElementById("email-to").textContent,
                        document.getElementById("email-subject").textContent);
                    fetchPending();
                }
            } catch (err) {
                alert("Error: " + err);
                document.getElementById("btn-approve").disabled = false;
                document.getElementById("btn-reject").disabled = false;
            }
        }

        function addToHistory(action, to, subject) {
            const container = document.getElementById("history-container");
            const list = document.getElementById("history-list");
            container.style.display = "block";

            const item = document.createElement("div");
            item.className = "history-item " + action;
            item.innerHTML = (action === "sent" ? "✓ Sent to " : "✗ Rejected: ") +
                "<strong>" + escapeHtml(to) + "</strong> - " + escapeHtml(subject) +
                " <span style='color:#999;font-size:12px;'>" + new Date().toLocaleTimeString() + "</span>";
            list.insertBefore(item, list.firstChild);
        }

        function escapeHtml(text) {
            const div = document.createElement("div");
            div.textContent = text;
            return div.innerHTML;
        }

        // Initial fetch
        fetchPending();
    </script>
</body>
</html>
`

func NewGmailServer() (*GmailServer, error) {
	ctx := context.Background()

	// Get credentials from separate environment variables
	clientID := os.Getenv("GMAIL_CLIENT_ID")
	clientSecret := os.Getenv("GMAIL_CLIENT_SECRET")

	if clientID == "" {
		return nil, fmt.Errorf("GMAIL_CLIENT_ID environment variable not set")
	}
	if clientSecret == "" {
		return nil, fmt.Errorf("GMAIL_CLIENT_SECRET environment variable not set")
	}

	// Create OAuth config from the client ID and secret
	config := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  "http://localhost:9876",
		Scopes:       []string{gmail.GmailReadonlyScope, gmail.GmailComposeScope},
		Endpoint:     google.Endpoint,
	}

	// Get token from file or perform OAuth flow
	token, err := getToken(config)
	if err != nil {
		return nil, fmt.Errorf("unable to get token: %v", err)
	}

	// Create Gmail service
	client := config.Client(ctx, token)
	service, err := gmail.NewService(ctx, googleOption.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("unable to create Gmail service: %v", err)
	}

	return &GmailServer{
		service: service,
		userID:  "me",
	}, nil
}

// getToken retrieves a token from a local file or initiates OAuth flow
func getToken(config *oauth2.Config) (*oauth2.Token, error) {
	tokenFile := getAppFilePath("token.json")

	// Try to load existing token
	token, err := tokenFromFile(tokenFile)
	if err != nil {
		log.Printf("No valid token file found (%v), starting OAuth flow...", err)
		return performOAuthFlow(config, tokenFile)
	}

	// Validate the token by testing it with a simple Gmail API call
	log.Println("Validating existing token...")
	if !isTokenValid(token) {
		log.Println("Existing token is invalid or expired, starting OAuth flow...")
		return performOAuthFlow(config, tokenFile)
	}

	log.Println("✅ Using existing valid token")
	return token, nil
}

// isTokenValid tests if a token is valid by making a simple API call
func isTokenValid(token *oauth2.Token) bool {
	// Create a temporary client to test the token
	config := &oauth2.Config{
		ClientID:     "",
		ClientSecret: "",
		Endpoint:     google.Endpoint,
		Scopes:       []string{gmail.GmailReadonlyScope, gmail.GmailComposeScope},
	}

	client := config.Client(context.Background(), token)
	service, err := gmail.NewService(context.Background(), googleOption.WithHTTPClient(client))
	if err != nil {
		return false
	}

	// Try a simple API call to verify the token works
	_, err = service.Users.GetProfile("me").Do()
	return err == nil
}

// performOAuthFlow handles the OAuth flow and saves the token
func performOAuthFlow(config *oauth2.Config, tokenFile string) (*oauth2.Token, error) {
	token, err := getTokenFromWeb(config)
	if err != nil {
		return nil, err
	}

	// Save token for next time
	saveToken(tokenFile, token)
	return token, nil
}

// getTokenFromWeb requests a token from the web, then returns the retrieved token
func getTokenFromWeb(config *oauth2.Config) (*oauth2.Token, error) {
	// Create a channel to receive the authorization code
	codeChan := make(chan string)
	errChan := make(chan error)

	// Start a temporary HTTP server to catch the OAuth callback
	server := &http.Server{Addr: ":9876"}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errChan <- fmt.Errorf("no code in callback")
			return
		}

		// Send success page to user
		fmt.Fprint(w, `
<!DOCTYPE html>
<html>
<head>
    <title>Gmail MCP Server - Authorization Complete</title>
    <style>
        body { font-family: Arial, sans-serif; text-align: center; margin-top: 50px; }
        .success { color: green; font-size: 18px; }
    </style>
</head>
<body>
    <h1>Authorization Successful!</h1>
    <p class="success">✅ You can now close this browser window and return to your terminal.</p>
    <p>Your Gmail MCP Server is now configured.</p>
</body>
</html>`)

		// Send the code back to the main flow
		codeChan <- code
	})

	// Start server in a goroutine
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("failed to start callback server: %v", err)
		}
	}()

	// Wait a moment for server to start
	time.Sleep(100 * time.Millisecond)

	// Update the redirect URI to point to our local server
	config.RedirectURL = "http://localhost:9876"

	// Generate the authorization URL
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)

	fmt.Println("Opening browser for authorization...")
	fmt.Printf("If browser doesn't open automatically, go to: %v\n", authURL)

	// Try to open browser automatically
	openBrowser(authURL)

	// Wait for either the code or an error
	var authCode string
	select {
	case authCode = <-codeChan:
		// Success! We got the code
	case err := <-errChan:
		return nil, fmt.Errorf("authorization failed: %v", err)
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("authorization timed out after 5 minutes")
	}

	// Shutdown the temporary server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)

	// Exchange the code for a token
	token, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve token from web: %v", err)
	}

	fmt.Println("✅ Authorization successful! Token saved.")
	return token, nil
}

// openBrowser tries to open the URL in the default browser
func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}

	if err != nil {
		fmt.Printf("Could not open browser automatically: %v\n", err)
	}
}

// tokenFromFile retrieves a token from a local file
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	token := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(token)
	return token, err
}

// saveToken saves a token to a file path
func saveToken(path string, token *oauth2.Token) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Printf("Unable to cache oauth token: %v", err)
		return
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

// SearchThreads searches Gmail threads based on a query
func (g *GmailServer) SearchThreads(ctx context.Context, query string, maxResults int64) (*mcp.CallToolResult, error) {
	if maxResults <= 0 {
		maxResults = 10
	}

	threads, err := g.service.Users.Threads.List(g.userID).Q(query).MaxResults(maxResults).Do()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to search threads: %v", err)), nil
	}

	var results []map[string]interface{}
	for _, thread := range threads.Threads {
		// Get thread details
		threadDetail, err := g.service.Users.Threads.Get(g.userID, thread.Id).Do()
		if err != nil {
			continue
		}

		if len(threadDetail.Messages) == 0 {
			continue
		}

		firstMessage := threadDetail.Messages[0]
		var subject, from, snippet string

		// Extract headers
		for _, header := range firstMessage.Payload.Headers {
			switch header.Name {
			case "Subject":
				subject = header.Value
			case "From":
				from = header.Value
			}
		}

		// Use Gmail's built-in snippet for fast browsing (typically ~150 characters)
		snippet = firstMessage.Snippet

		// Collect attachment information from all messages in the thread
		var allAttachments []map[string]interface{}
		for _, message := range threadDetail.Messages {
			attachments := extractAttachmentInfo(message)
			for _, attachment := range attachments {
				// Add message ID to each attachment for reference
				attachment["messageId"] = message.Id
				allAttachments = append(allAttachments, attachment)
			}
		}

		// Get existing drafts for this thread
		existingDrafts, err := g.getThreadDrafts(thread.Id)
		if err != nil {
			log.Printf("Warning: Failed to get drafts for thread %s: %v", thread.Id, err)
			existingDrafts = []map[string]interface{}{}
		}

		threadResult := map[string]interface{}{
			"threadId":     thread.Id,
			"subject":      subject,
			"from":         from,
			"snippet":      snippet,
			"messageCount": len(threadDetail.Messages),
		}

		// Only include attachments if there are any
		if len(allAttachments) > 0 {
			threadResult["attachments"] = allAttachments
		}

		// Only include drafts if there are any
		if len(existingDrafts) > 0 {
			threadResult["drafts"] = existingDrafts
		}

		results = append(results, threadResult)
	}

	resultJSON, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(resultJSON)), nil
}

// getThreadDrafts retrieves existing drafts for a specific thread
func (g *GmailServer) getThreadDrafts(threadID string) ([]map[string]interface{}, error) {
	var drafts []map[string]interface{}

	// List all drafts for the user
	draftsList, err := g.service.Users.Drafts.List(g.userID).Do()
	if err != nil {
		return drafts, fmt.Errorf("failed to list drafts: %v", err)
	}

	// Check each draft to see if it belongs to this thread
	for _, draft := range draftsList.Drafts {
		// Get the full draft details
		fullDraft, err := g.service.Users.Drafts.Get(g.userID, draft.Id).Do()
		if err != nil {
			continue // Skip drafts we can't access
		}

		// Check if this draft belongs to the specified thread
		if fullDraft.Message != nil && fullDraft.Message.ThreadId == threadID {
			draftInfo := map[string]interface{}{
				"draftId":  fullDraft.Id,
				"threadId": fullDraft.Message.ThreadId,
			}

			// Extract subject and snippet if available
			if fullDraft.Message.Payload != nil {
				for _, header := range fullDraft.Message.Payload.Headers {
					if header.Name == "Subject" {
						draftInfo["subject"] = header.Value
						break
					}
				}

				// Extract draft body/snippet
				if body := extractEmailBody(fullDraft.Message); body != "" {
					// Truncate to snippet length
					snippet := body
					if len(snippet) > 200 {
						snippet = snippet[:200] + "..."
					}
					draftInfo["snippet"] = snippet
				}
			}

			drafts = append(drafts, draftInfo)
		}
	}

	return drafts, nil
}

// CreateDraft creates a Gmail draft or updates existing draft if one exists for the thread
func (g *GmailServer) CreateDraft(ctx context.Context, to, subject, body string, threadID string) (*mcp.CallToolResult, error) {
	var message gmail.Message

	// Build the email message
	headers := fmt.Sprintf("To: %s\r\n", to)

	if threadID != "" {
		// Set the thread ID on the message for proper threading
		message.ThreadId = threadID

		// Ensure subject has "Re:" prefix for replies
		if !strings.HasPrefix(strings.ToLower(subject), "re:") {
			subject = "Re: " + subject
		}

		// For replies, we need to set the In-Reply-To and References headers
		thread, err := g.service.Users.Threads.Get(g.userID, threadID).Do()
		if err == nil && len(thread.Messages) > 0 {
			lastMessage := thread.Messages[len(thread.Messages)-1]
			var messageID string
			var references string

			// Extract Message-ID and References from the last message
			for _, header := range lastMessage.Payload.Headers {
				switch header.Name {
				case "Message-ID":
					messageID = header.Value
				case "References":
					references = header.Value
				}
			}

			if messageID != "" {
				headers += fmt.Sprintf("In-Reply-To: %s\r\n", messageID)

				// Build References header (previous references + last message ID)
				if references != "" {
					headers += fmt.Sprintf("References: %s %s\r\n", references, messageID)
				} else {
					headers += fmt.Sprintf("References: %s\r\n", messageID)
				}
			}
		}

		// Check for existing drafts in this thread and update if found
		existingDrafts, err := g.getThreadDrafts(threadID)
		if err == nil && len(existingDrafts) > 0 {
			// Assume only one draft per thread (as requested)
			existingDraftID := existingDrafts[0]["draftId"].(string)

			headers += fmt.Sprintf("Subject: %s\r\n", subject)
			rawMessage := headers + "\r\n" + body
			message.Raw = base64.URLEncoding.EncodeToString([]byte(rawMessage))

			draft := &gmail.Draft{
				Id:      existingDraftID,
				Message: &message,
			}

			updatedDraft, err := g.service.Users.Drafts.Update(g.userID, existingDraftID, draft).Do()
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Failed to update existing draft: %v", err)), nil
			}

			result := map[string]interface{}{
				"draftId": updatedDraft.Id,
				"message": "Draft updated successfully (existing draft was overwritten)",
				"action":  "updated",
				"to":      to,
				"subject": subject,
			}

			resultJSON, _ := json.MarshalIndent(result, "", "  ")
			return mcp.NewToolResultText(string(resultJSON)), nil
		}
	}

	// No existing draft found or no thread ID, create new draft
	headers += fmt.Sprintf("Subject: %s\r\n", subject)
	rawMessage := headers + "\r\n" + body

	// Gmail API requires base64url-encoded raw message
	message.Raw = base64.URLEncoding.EncodeToString([]byte(rawMessage))

	draft := &gmail.Draft{
		Message: &message,
	}

	createdDraft, err := g.service.Users.Drafts.Create(g.userID, draft).Do()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to create draft: %v", err)), nil
	}

	result := map[string]interface{}{
		"draftId": createdDraft.Id,
		"message": "Draft created successfully",
		"action":  "created",
		"to":      to,
		"subject": subject,
	}

	resultJSON, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(resultJSON)), nil
}

// GetUserProfile gets the user's Gmail profile information
func (g *GmailServer) GetUserProfile() (*gmail.Profile, error) {
	profile, err := g.service.Users.GetProfile(g.userID).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get user profile: %v", err)
	}
	return profile, nil
}

// GeneratePersonalEmailStyleGuide analyzes sent emails and generates a tone personalization file
func GeneratePersonalEmailStyleGuide(gmailServer *GmailServer) error {
	log.Println("Generating personal email style guide from sent emails...")

	// Get OpenAI API key
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENAI_API_KEY environment variable not set")
	}

	// Create OpenAI client
	client := openai.NewClient(option.WithAPIKey(apiKey))

	// Get user profile information
	log.Println("Fetching user profile...")
	profile, err := gmailServer.GetUserProfile()
	if err != nil {
		log.Printf("Warning: Could not fetch user profile: %v", err)
		profile = &gmail.Profile{EmailAddress: "unknown@example.com"}
	}

	// Get sent emails
	log.Println("Fetching sent emails...")
	messages, err := gmailServer.service.Users.Messages.List(gmailServer.userID).Q("in:sent").MaxResults(50).Do()
	if err != nil {
		return fmt.Errorf("failed to fetch sent messages: %v", err)
	}

	var emailBodies []string
	var emailHeaders []map[string]string
	for _, msg := range messages.Messages {
		// Get full message
		fullMsg, err := gmailServer.service.Users.Messages.Get(gmailServer.userID, msg.Id).Do()
		if err != nil {
			continue
		}

		// Extract email body
		body := extractEmailBody(fullMsg)
		if body != "" && len(body) > 50 { // Only include substantial emails
			emailBodies = append(emailBodies, body)

			// Extract headers for additional context
			headers := make(map[string]string)
			if fullMsg.Payload != nil {
				for _, header := range fullMsg.Payload.Headers {
					if header.Name == "Subject" || header.Name == "To" || header.Name == "From" {
						headers[header.Name] = header.Value
					}
				}
			}
			emailHeaders = append(emailHeaders, headers)
		}

		// Limit to avoid hitting token limits
		if len(emailBodies) >= 25 {
			break
		}
	}

	if len(emailBodies) == 0 {
		return fmt.Errorf("no sent emails found to analyze")
	}

	log.Printf("Analyzing %d sent emails...", len(emailBodies))

	// Build comprehensive email samples with context
	var emailSamples []string
	for i, body := range emailBodies {
		sample := fmt.Sprintf("Email %d:\n", i+1)
		if i < len(emailHeaders) {
			if subject, ok := emailHeaders[i]["Subject"]; ok {
				sample += fmt.Sprintf("Subject: %s\n", subject)
			}
			if to, ok := emailHeaders[i]["To"]; ok {
				sample += fmt.Sprintf("To: %s\n", to)
			}
		}
		sample += fmt.Sprintf("Body: %s", body)
		emailSamples = append(emailSamples, sample)
	}

	samplesText := strings.Join(emailSamples, "\n\n---\n\n")

	// Concise, focused prompt that encourages specificity
	prompt := fmt.Sprintf(`Analyze these %d emails from %s to create a concise, specific email style guide.

EMAILS:
%s

Create a markdown guide with:

1. **USER BACKGROUND**: Infer their role, industry, expertise from email content/recipients
2. **WRITING PATTERNS**: Specific words/phrases they actually use (not generic advice)
3. **STRUCTURE**: How they organize emails (greeting→body→closing patterns)
4. **TONE**: Their actual communication style with examples
5. **SIGNATURE ELEMENTS**: Unique characteristics that make emails sound like them

Be specific and actionable. Avoid generic advice. Focus on what makes THIS person's emails distinctive.

Start with "# Personal Email Style Guide for %s"`, len(emailBodies), profile.EmailAddress, samplesText, profile.EmailAddress)

	// Call OpenAI API
	log.Println("Generating personal email style guide with OpenAI...")
	completion, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			{
				OfUser: &openai.ChatCompletionUserMessageParam{
					Content: openai.ChatCompletionUserMessageParamContentUnion{
						OfString: openai.String(prompt),
					},
				},
			},
		},
		Model:       shared.ChatModelGPT4o,
		Temperature: openai.Float(0.3), // Lower temperature for more focused, consistent output
	})
	if err != nil {
		return fmt.Errorf("failed to generate style guide: %v", err)
	}

	// Get the generated content
	if len(completion.Choices) == 0 {
		return fmt.Errorf("no response from OpenAI")
	}

	styleGuide := completion.Choices[0].Message.Content

	// Save to file
	styleFilePath := getAppFilePath("personal-email-style-guide.md")
	err = os.WriteFile(styleFilePath, []byte(styleGuide), 0644)
	if err != nil {
		return fmt.Errorf("failed to write personal email style guide file: %v", err)
	}

	log.Printf("Successfully generated personal-email-style-guide.md at: %s", styleFilePath)
	return nil
}

// extractEmailBody extracts readable text from a Gmail message, preserving links and semantic information
func extractEmailBody(msg *gmail.Message) string {
	if msg.Payload == nil {
		return ""
	}

	// Try to get content from message body or parts
	var plainTextContent, htmlContent string

	// Check if there's direct body content
	if msg.Payload.Body != nil && msg.Payload.Body.Data != "" {
		decoded, err := decodeEmailContent(msg.Payload.Body.Data)
		if err == nil {
			if msg.Payload.MimeType == "text/html" {
				htmlContent = decoded
			} else {
				plainTextContent = decoded
			}
		}
	}

	// For multipart messages, extract from parts
	if len(msg.Payload.Parts) > 0 {
		plainFromParts, htmlFromParts := extractFromParts(msg.Payload.Parts)
		if plainFromParts != "" {
			plainTextContent = plainFromParts
		}
		if htmlFromParts != "" {
			htmlContent = htmlFromParts
		}
	}

	// Prefer HTML content when available since it contains more semantic information
	if htmlContent != "" {
		return extractTextAndLinksFromHTML(htmlContent)
	}

	return plainTextContent
}

// extractFromParts recursively extracts both plain text and HTML content from message parts
func extractFromParts(parts []*gmail.MessagePart) (plainText, htmlText string) {
	for _, part := range parts {
		if part.Body != nil && part.Body.Data != "" {
			decoded, err := decodeEmailContent(part.Body.Data)
			if err != nil {
				continue
			}

			switch part.MimeType {
			case "text/plain":
				if plainText == "" { // Take the first plain text part
					plainText = decoded
				}
			case "text/html":
				if htmlText == "" { // Take the first HTML part
					htmlText = decoded
				}
			}
		}

		// Recursively check nested parts
		if len(part.Parts) > 0 {
			nestedPlain, nestedHTML := extractFromParts(part.Parts)
			if plainText == "" && nestedPlain != "" {
				plainText = nestedPlain
			}
			if htmlText == "" && nestedHTML != "" {
				htmlText = nestedHTML
			}
		}
	}
	return plainText, htmlText
}

// decodeEmailContent decodes base64url or base64 encoded email content
func decodeEmailContent(data string) (string, error) {
	// Try base64url decoding first (Gmail's preferred encoding)
	decoded, err := base64.URLEncoding.DecodeString(data)
	if err != nil {
		// Try standard base64 if URL encoding fails
		decoded, err = base64.StdEncoding.DecodeString(data)
		if err != nil {
			return "", err
		}
	}
	return string(decoded), nil
}

// extractTextAndLinksFromHTML uses html-to-markdown library to convert HTML to proper markdown with preserved links
func extractTextAndLinksFromHTML(htmlContent string) string {
	// Use JohannesKaufmann/html-to-markdown/v2 library for proper markdown conversion
	markdown, err := htmltomarkdown.ConvertString(htmlContent)
	if err != nil {
		// Fallback to returning the HTML as-is if conversion fails
		return htmlContent
	}

	return strings.TrimSpace(markdown)
}

// extractAttachmentInfo extracts attachment information from a Gmail message
func extractAttachmentInfo(message *gmail.Message) []map[string]interface{} {
	var attachments []map[string]interface{}

	if message.Payload == nil {
		return attachments
	}

	// Check payload parts for attachments
	extractAttachmentsFromParts(message.Payload.Parts, &attachments)

	return attachments
}

// extractAttachmentsFromParts recursively extracts attachment info from message parts
func extractAttachmentsFromParts(parts []*gmail.MessagePart, attachments *[]map[string]interface{}) {
	for _, part := range parts {
		// Check if this part is an attachment
		if part.Body != nil && part.Body.AttachmentId != "" {
			filename := part.Filename
			if filename == "" {
				filename = "unnamed_attachment"
			}

			attachment := map[string]interface{}{
				"attachmentId": part.Body.AttachmentId,
				"filename":     filename,
				"mimeType":     part.MimeType,
				"size":         part.Body.Size,
			}

			// Mark if this is a document we can extract text from
			if isExtractableDocument(part.MimeType, filename) {
				attachment["extractable"] = true
			}

			*attachments = append(*attachments, attachment)
		}

		// Recursively check nested parts
		if len(part.Parts) > 0 {
			extractAttachmentsFromParts(part.Parts, attachments)
		}
	}
}

// isExtractableDocument checks if we can extract text from this document type
func isExtractableDocument(mimeType, filename string) bool {
	// Check MIME type
	switch mimeType {
	case "application/pdf":
		return true
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return true
	case "text/plain":
		return true
	}

	// Check file extension as fallback
	lowerFilename := strings.ToLower(filename)
	return strings.HasSuffix(lowerFilename, ".pdf") ||
		strings.HasSuffix(lowerFilename, ".docx") ||
		strings.HasSuffix(lowerFilename, ".txt")
}

// ExtractAttachmentText safely extracts text content from an email attachment
func (g *GmailServer) ExtractAttachmentText(ctx context.Context, messageID, attachmentID string) (*mcp.CallToolResult, error) {
	// Get the message to extract attachment metadata
	message, err := g.service.Users.Messages.Get(g.userID, messageID).Do()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get message: %v", err)), nil
	}

	// Debug: Print all attachment IDs found in this message
	log.Printf("Looking for attachment ID: %s", attachmentID)
	allAttachments := extractAttachmentInfo(message)
	log.Printf("Found %d attachments in message:", len(allAttachments))
	for i, att := range allAttachments {
		log.Printf("  Attachment %d: ID=%v, filename=%v", i, att["attachmentId"], att["filename"])
	}

	// Find the attachment part to get metadata
	var attachmentPart *gmail.MessagePart
	findAttachmentPart(message.Payload.Parts, attachmentID, &attachmentPart)

	if attachmentPart == nil {
		return mcp.NewToolResultError(fmt.Sprintf("Attachment not found in message. Available attachments: %v", allAttachments)), nil
	}

	// Get the attachment data
	attachment, err := g.service.Users.Messages.Attachments.Get(g.userID, messageID, attachmentID).Do()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get attachment: %v", err)), nil
	}

	// Decode the attachment data
	data, err := base64.URLEncoding.DecodeString(attachment.Data)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to decode attachment data: %v", err)), nil
	}

	// Extract text based on MIME type
	text, err := extractTextFromBytes(data, attachmentPart.MimeType, attachmentPart.Filename)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to extract text: %v", err)), nil
	}

	result := map[string]interface{}{
		"messageId":    messageID,
		"attachmentId": attachmentID,
		"filename":     attachmentPart.Filename,
		"mimeType":     attachmentPart.MimeType,
		"textContent":  text,
		"extractedAt":  time.Now().Format(time.RFC3339),
	}

	resultJSON, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(resultJSON)), nil
}

// findAttachmentPart recursively finds the attachment part by attachment ID
func findAttachmentPart(parts []*gmail.MessagePart, attachmentID string, result **gmail.MessagePart) {
	for _, part := range parts {
		if part.Body != nil && part.Body.AttachmentId == attachmentID {
			*result = part
			return
		}
		if len(part.Parts) > 0 {
			findAttachmentPart(part.Parts, attachmentID, result)
		}
	}
}

// extractTextFromBytes extracts text from attachment bytes based on MIME type
func extractTextFromBytes(data []byte, mimeType, filename string) (string, error) {
	switch mimeType {
	case "application/pdf":
		return extractPDFText(data)
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return extractDOCXText(data)
	case "text/plain":
		return string(data), nil
	default:
		// Try to infer from filename
		lowerFilename := strings.ToLower(filename)
		if strings.HasSuffix(lowerFilename, ".pdf") {
			return extractPDFText(data)
		} else if strings.HasSuffix(lowerFilename, ".docx") {
			return extractDOCXText(data)
		} else if strings.HasSuffix(lowerFilename, ".txt") {
			return string(data), nil
		}
		return "", fmt.Errorf("unsupported file type: %s", mimeType)
	}
}

// extractPDFText safely extracts text from PDF bytes
func extractPDFText(data []byte) (string, error) {
	reader := bytes.NewReader(data)

	// Open PDF reader
	pdfReader, err := pdf.NewReader(reader, int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("failed to open PDF: %v", err)
	}

	var textContent strings.Builder
	numPages := pdfReader.NumPage()

	// Limit to first 50 pages to avoid excessive processing
	maxPages := numPages
	if maxPages > 50 {
		maxPages = 50
	}

	for i := 1; i <= maxPages; i++ {
		page := pdfReader.Page(i)
		if page.V.IsNull() {
			continue
		}

		// Extract text with empty font map (safe extraction)
		text, err := page.GetPlainText(map[string]*pdf.Font{})
		if err != nil {
			// Continue with other pages if one fails
			continue
		}

		textContent.WriteString(text)
		textContent.WriteString("\n\n")
	}

	extractedText := textContent.String()
	if len(extractedText) == 0 {
		return "", fmt.Errorf("no text could be extracted from PDF")
	}

	// Add truncation notice if we hit the page limit
	if numPages > 50 {
		extractedText += fmt.Sprintf("\n\n[Note: PDF has %d pages total, but only first 50 pages were processed for safety]", numPages)
	}

	return extractedText, nil
}

// extractDOCXText safely extracts text from DOCX bytes
func extractDOCXText(data []byte) (string, error) {
	// Create a temporary file since the docx library works with files
	tempFile, err := os.CreateTemp("", "docx_extract_*.docx")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// Write data to temp file
	if _, err := tempFile.Write(data); err != nil {
		return "", fmt.Errorf("failed to write temp file: %v", err)
	}
	tempFile.Close()

	// Read DOCX from the temporary file
	doc, err := docx.ReadDocxFile(tempFile.Name())
	if err != nil {
		return "", fmt.Errorf("failed to open DOCX: %v", err)
	}

	// Get the raw content (which may be XML)
	rawContent := doc.Editable().GetContent()
	if len(rawContent) == 0 {
		return "", fmt.Errorf("no text could be extracted from DOCX")
	}

	// Try to extract plain text from XML if the content looks like XML
	if strings.HasPrefix(strings.TrimSpace(rawContent), "<?xml") || strings.HasPrefix(strings.TrimSpace(rawContent), "<") {
		plainText := extractTextFromXML(rawContent)
		if len(plainText) > 0 {
			return plainText, nil
		}
		// If XML parsing fails, fall back to raw content
	}

	return rawContent, nil
}

// extractTextFromXML extracts plain text content from DOCX XML
func extractTextFromXML(xmlContent string) string {
	var textParts []string

	// Create a decoder for the XML content
	decoder := xml.NewDecoder(strings.NewReader(xmlContent))

	// Track if we're inside a <w:t> element
	var insideTextElement bool

	for {
		// Read the next token
		token, err := decoder.Token()
		if err != nil {
			break // End of document or error
		}

		switch t := token.(type) {
		case xml.StartElement:
			// Check if this is a text element
			if t.Name.Local == "t" && t.Name.Space == "http://schemas.openxmlformats.org/wordprocessingml/2006/main" {
				insideTextElement = true
			}
		case xml.EndElement:
			// Check if we're leaving a text element
			if t.Name.Local == "t" && t.Name.Space == "http://schemas.openxmlformats.org/wordprocessingml/2006/main" {
				insideTextElement = false
			}
		case xml.CharData:
			// If we're inside a text element, collect the text
			if insideTextElement {
				text := strings.TrimSpace(string(t))
				if text != "" {
					textParts = append(textParts, text)
				}
			}
		}
	}

	// Join all text parts with spaces and clean up
	result := strings.Join(textParts, " ")

	// Clean up extra whitespace while preserving meaningful breaks
	// Split by multiple spaces and rejoin with single spaces
	words := strings.Fields(result)
	return strings.Join(words, " ")
}

// getAppDataDir returns the application data directory
func getAppDataDir() string {
	var appDataDir string

	if runtime.GOOS == "windows" {
		// Windows: %APPDATA%\auto-gmail
		appDataDir = filepath.Join(os.Getenv("APPDATA"), "auto-gmail")
	} else {
		// Mac/Linux: ~/.auto-gmail
		homeDir, err := os.UserHomeDir()
		if err != nil {
			log.Printf("Warning: Could not get home directory: %v", err)
			return "."
		}
		appDataDir = filepath.Join(homeDir, ".auto-gmail")
	}

	// Ensure the directory exists
	if err := os.MkdirAll(appDataDir, 0755); err != nil {
		log.Printf("Warning: Could not create app data directory: %v", err)
		return "."
	}

	return appDataDir
}

// getAppFilePath returns an absolute path in the app data directory
func getAppFilePath(filename string) string {
	return filepath.Join(getAppDataDir(), filename)
}

// ensureStyleGuideExists checks if the style guide exists and auto-generates it if needed
func ensureStyleGuideExists(gmailServer *GmailServer) error {
	toneFilePath := getAppFilePath("personal-email-style-guide.md")

	// Check if file already exists
	if _, err := os.Stat(toneFilePath); err == nil {
		return nil // File exists, nothing to do
	}

	// File doesn't exist, try to auto-generate
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("personal email style guide not found at %s and OPENAI_API_KEY not set. Please either set OPENAI_API_KEY for auto-generation or create the file manually", toneFilePath)
	}

	log.Println("📝 Style guide not found, auto-generating from your sent emails...")
	if err := GeneratePersonalEmailStyleGuide(gmailServer); err != nil {
		return fmt.Errorf("personal email style guide not found at %s and auto-generation failed: %v. Please create the file manually or set OPENAI_API_KEY", toneFilePath, err)
	}

	log.Println("✅ Personal email style guide auto-generated successfully!")
	return nil
}

func main() {
	// Parse command line arguments for transport mode
	var useHTTP = false
	var port = "8080"

	if len(os.Args) > 1 {
		if os.Args[1] == "--http" {
			useHTTP = true
		}
		if len(os.Args) > 2 {
			port = os.Args[2]
		}
	}

	// Load environment variables from .env file if it exists
	err := godotenv.Load()
	if err == nil {
		log.Printf("Loaded .env file")
	}

	// Show file locations early
	log.Printf("📁 App data directory: %s", getAppDataDir())
	log.Printf("🔑 Token file: %s", getAppFilePath("token.json"))
	log.Printf("📝 Style guide file: %s", getAppFilePath("personal-email-style-guide.md"))

	// Create Gmail server instance
	gmailServer, err := NewGmailServer()
	if err != nil {
		log.Fatalf("Failed to create Gmail server: %v", err)
	}

	// Auto-generate tone personalization file if it doesn't exist
	if err := ensureStyleGuideExists(gmailServer); err != nil {
		log.Printf("⚠️  %v", err)
	}

	// Initialize OOB approval session (Agent Cut-Out Pattern)
	approvalSession, err = NewApprovalSession()
	if err != nil {
		log.Fatalf("Failed to create approval session: %v", err)
	}

	// Start the OOB approval web server
	StartOOBServer(gmailServer)

	// Print the dashboard URL prominently
	log.Println("")
	log.Println("═══════════════════════════════════════════════════════════════")
	log.Println("📤 OOB APPROVAL DASHBOARD (Agent Cut-Out Pattern)")
	log.Println("═══════════════════════════════════════════════════════════════")
	log.Printf("   http://localhost:%d/outbox/%s", oobServerPort, approvalSession.ID)
	log.Println("")
	log.Println("   Open this URL in your browser to view/approve outgoing emails.")
	log.Println("   The agent CANNOT see or influence this approval process.")
	log.Println("")
	log.Println("   NOTE: Primary approval is via mobile push notification.")
	log.Println("   Make sure gmail-approval-daemon is running for mobile push.")
	log.Println("═══════════════════════════════════════════════════════════════")
	log.Println("")

	// Create MCP server
	mcpServer := server.NewMCPServer(
		"Gmail MCP Server",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithPromptCapabilities(true),
	)

	// Add email tone resource
	toneResource := mcp.NewResource(
		"file://personal-email-style-guide",
		"Personal Email Style Guide",
		mcp.WithResourceDescription("Instructions on how to write emails in the user's personal style and tone"),
		mcp.WithMIMEType("text/markdown"),
	)

	mcpServer.AddResource(toneResource, func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		// Try to read from personal-email-style-guide.md file in app data directory
		toneFilePath := getAppFilePath("personal-email-style-guide.md")
		content, err := os.ReadFile(toneFilePath)
		if err != nil {
			// If file doesn't exist, try to generate it automatically
			if os.IsNotExist(err) {
				if genErr := ensureStyleGuideExists(gmailServer); genErr != nil {
					return nil, genErr
				}
				// Try reading again after generation
				content, err = os.ReadFile(toneFilePath)
				if err != nil {
					return nil, fmt.Errorf("failed to read generated style guide: %v", err)
				}
			} else {
				return nil, fmt.Errorf("failed to read style guide at %s: %v", toneFilePath, err)
			}
		}

		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      "file://personal-email-style-guide",
				MIMEType: "text/markdown",
				Text:     string(content),
			},
		}, nil
	})

	// Add administrative prompts
	generateTonePrompt := mcp.NewPrompt(
		"generate-email-tone",
		mcp.WithPromptDescription("Generate email tone personalization by analyzing your sent emails"),
	)

	mcpServer.AddPrompt(generateTonePrompt, func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		// Check if OPENAI_API_KEY is available
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			return &mcp.GetPromptResult{
				Messages: []mcp.PromptMessage{
					mcp.NewPromptMessage(
						mcp.RoleUser,
						mcp.NewTextContent("❌ Cannot generate tone: OPENAI_API_KEY environment variable not set"),
					),
				},
			}, nil
		}

		// Generate tone personalization
		err := GeneratePersonalEmailStyleGuide(gmailServer)
		if err != nil {
			return &mcp.GetPromptResult{
				Messages: []mcp.PromptMessage{
					mcp.NewPromptMessage(
						mcp.RoleUser,
						mcp.NewTextContent(fmt.Sprintf("❌ Failed to generate tone: %v", err)),
					),
				},
			}, nil
		}

		toneFilePath := getAppFilePath("personal-email-style-guide.md")
		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				mcp.NewPromptMessage(
					mcp.RoleUser,
					mcp.NewTextContent(fmt.Sprintf("✅ Successfully generated personal email style guide at: %s\n\nYou can now use the file://personal-email-style-guide resource for personalized email writing.", toneFilePath)),
				),
			},
		}, nil
	})

	statusPrompt := mcp.NewPrompt(
		"server-status",
		mcp.WithPromptDescription("Show Gmail MCP server status and file locations"),
	)

	mcpServer.AddPrompt(statusPrompt, func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		// Check file statuses
		tokenPath := getAppFilePath("token.json")
		tonePath := getAppFilePath("personal-email-style-guide.md")

		tokenExists := "❌ Not found"
		if _, err := os.Stat(tokenPath); err == nil {
			tokenExists = "✅ Found"
		}

		toneExists := "❌ Not found"
		if _, err := os.Stat(tonePath); err == nil {
			toneExists = "✅ Found"
		}

		statusMessage := fmt.Sprintf("📊 **Gmail MCP Server Status**\n\n📁 **App Data Directory:** %s\n\n🔑 **Token File:** %s\n   Status: %s\n\n📝 **Style Guide File:** %s\n   Status: %s\n\n🛠️ **Available Commands:**\n- Use /generate-email-tone to create email tone personalization\n- Use tools: search_threads (includes drafts), create_draft (create/update), extract_attachment_by_filename\n- Use resource: file://personal-email-style-guide",
			getAppDataDir(), tokenPath, tokenExists, tonePath, toneExists)

		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				mcp.NewPromptMessage(
					mcp.RoleUser,
					mcp.NewTextContent(statusMessage),
				),
			},
		}, nil
	})

	// Add Search Threads tool
	searchThreadsTool := mcp.NewTool("search_threads",
		mcp.WithDescription(`Search Gmail threads using Gmail's powerful query syntax.

GMAIL SEARCH OPERATORS:
Basic Filters:
  from:amy@example.com           - Find emails from specific sender
  to:me                          - Find emails sent to specific recipient  
  cc:john@example.com            - Find emails with specific CC
  subject:"quarterly review"     - Find emails with specific subject text
  
Date/Time Filters:
  after:2025/06/01               - Emails after specific date
  before:2025/06/07              - Emails before specific date  
  older_than:7d                  - Older than 7 days (use d/m/y)
  newer_than:2m                  - Newer than 2 months
  
Content & Attachments:
  has:attachment                 - Has any attachment
  filename:pdf                   - Has PDF attachment
  filename:report.txt            - Has specific filename
  has:youtube                    - Contains YouTube videos
  has:drive                      - Contains Google Drive files
  
Labels & Categories:
  label:important                - Has specific label
  category:promotions            - In specific category
  is:unread                      - Unread messages
  is:starred                     - Starred messages
  is:important                   - Marked important
  in:sent                        - In sent folder
  in:trash                       - In trash
  in:anywhere                    - Search everywhere including spam/trash
  
Advanced Operators:
  "exact phrase"                 - Search for exact phrase
  (dinner movie)                 - Group terms together
  holiday AROUND 10 vacation     - Words within 10 words of each other
  from:amy OR from:bob           - Either condition (use OR or { })
  from:amy AND to:david          - Both conditions
  dinner -movie                  - Include dinner, exclude movie
  +unicorn                       - Match word exactly
  
Size & Technical:
  larger:10M                     - Larger than 10MB
  smaller:1M                     - Smaller than 1MB
  rfc822msgid:<id@example.com>   - Specific message ID
  list:info@example.com          - From mailing list
  deliveredto:user@example.com   - Delivered to specific address

EXAMPLE QUERIES:
  "is:unread"                    - All unread emails
  "from:support@github.com"      - All emails from GitHub
  "subject:invoice older_than:30d" - Old invoices
  "has:attachment filename:pdf"  - PDF attachments
  "from:boss@company.com is:unread" - Unread emails from boss
  "(urgent OR important) newer_than:1d" - Recent urgent/important emails`),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Gmail search query using the operators above (e.g., 'from:example@gmail.com', 'subject:meeting', 'is:unread')"),
		),
		mcp.WithNumber("max_results",
			mcp.Description("Maximum number of threads to return (default: 10)"),
		),
	)

	mcpServer.AddTool(searchThreadsTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError("query parameter is required and must be a string"), nil
		}

		maxResults := int64(10)
		args := req.GetArguments()
		if mr, ok := args["max_results"].(float64); ok {
			maxResults = int64(mr)
		}

		return gmailServer.SearchThreads(ctx, query, maxResults)
	})

	// Add Create Draft tool
	createDraftTool := mcp.NewTool("create_draft",
		mcp.WithDescription("Create a Gmail draft email or update an existing draft if one exists for the thread. When a thread_id is provided, this tool will check for existing drafts in that thread and overwrite them, allowing LLMs to iteratively modify draft content. Important: Before writing any email, always request the file://personal-email-style-guide resource to understand the user's writing style and preferences."),
		mcp.WithString("to",
			mcp.Required(),
			mcp.Description("Recipient email address"),
		),
		mcp.WithString("subject",
			mcp.Required(),
			mcp.Description("Email subject line"),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("Email body content"),
		),
		mcp.WithString("thread_id",
			mcp.Description("Thread ID if this is a reply (optional). If provided and a draft exists for this thread, the existing draft will be updated instead of creating a new one."),
		),
	)

	mcpServer.AddTool(createDraftTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		to, err := req.RequireString("to")
		if err != nil {
			return mcp.NewToolResultError("to parameter is required and must be a string"), nil
		}

		subject, err := req.RequireString("subject")
		if err != nil {
			return mcp.NewToolResultError("subject parameter is required and must be a string"), nil
		}

		body, err := req.RequireString("body")
		if err != nil {
			return mcp.NewToolResultError("body parameter is required and must be a string"), nil
		}

		threadID := ""
		args := req.GetArguments()
		if tid, ok := args["thread_id"].(string); ok {
			threadID = tid
		}

		return gmailServer.CreateDraft(ctx, to, subject, body, threadID)
	})

	// TEMPORARY HACK: Add personal email style guide as a tool
	// This is only needed until more MCP clients support resource-fetching properly
	// TODO: Remove this tool once resource support is more widespread
	getStyleGuideTool := mcp.NewTool("get_personal_email_style_guide",
		mcp.WithDescription("Get the user's personal email writing style guide. IMPORTANT: Always call this tool BEFORE drafting any emails to understand the user's writing style and tone. This is a temporary tool that will be removed once more agents support resource-fetching."),
	)

	mcpServer.AddTool(getStyleGuideTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Read the personal email style guide file
		styleFilePath := getAppFilePath("personal-email-style-guide.md")
		content, err := os.ReadFile(styleFilePath)
		if err != nil {
			if os.IsNotExist(err) {
				// Try to auto-generate if file doesn't exist
				if genErr := ensureStyleGuideExists(gmailServer); genErr != nil {
					return mcp.NewToolResultError(genErr.Error()), nil
				}
				// Try reading again after generation
				content, err = os.ReadFile(styleFilePath)
				if err != nil {
					return mcp.NewToolResultError(fmt.Sprintf("Failed to read generated style guide: %v", err)), nil
				}
			} else {
				return mcp.NewToolResultError(fmt.Sprintf("Failed to read style guide at %s: %v", styleFilePath, err)), nil
			}
		}

		return mcp.NewToolResultText(string(content)), nil
	})

	// Add Extract Attachment By Filename tool - more reliable than attachment ID
	extractByFilenameTool := mcp.NewTool("extract_attachment_by_filename",
		mcp.WithDescription("Safely extract text content from email attachments by filename (do not use attachment-id). Use search_threads first to find emails with attachments, then use this tool to extract readable text from specific files by name."),
		mcp.WithString("message_id",
			mcp.Required(),
			mcp.Description("The Gmail message ID containing the attachment (from search_threads results)"),
		),
		mcp.WithString("filename",
			mcp.Required(),
			mcp.Description("The filename of the attachment to extract (e.g., 'document.pdf', 'CV.docx')"),
		),
	)

	mcpServer.AddTool(extractByFilenameTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		messageID, err := req.RequireString("message_id")
		if err != nil {
			return mcp.NewToolResultError("message_id parameter is required and must be a string"), nil
		}

		filename, err := req.RequireString("filename")
		if err != nil {
			return mcp.NewToolResultError("filename parameter is required and must be a string"), nil
		}

		return gmailServer.ExtractAttachmentByFilename(ctx, messageID, filename)
	})

	// Add Fetch Email Bodies tool for selective full content retrieval
	fetchEmailBodiesTool := mcp.NewTool("fetch_email_bodies",
		mcp.WithDescription("Fetch full email bodies for specific threads after browsing with snippets. Can fetch multiple emails at once for efficient selective content retrieval."),
		mcp.WithString("thread_ids",
			mcp.Required(),
			mcp.Description("A comma-separated list of thread IDs to fetch full email content for (e.g., 'id1,id2,id3')"),
		),
	)

	mcpServer.AddTool(fetchEmailBodiesTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		threadIDsStr, err := req.RequireString("thread_ids")
		if err != nil {
			return mcp.NewToolResultError("thread_ids parameter is required and must be a string"), nil
		}

		// Split the comma-separated string into a slice
		threadIDs := strings.Split(threadIDsStr, ",")
		for i, id := range threadIDs {
			threadIDs[i] = strings.TrimSpace(id)
		}

		if len(threadIDs) == 0 || (len(threadIDs) == 1 && threadIDs[0] == "") {
			return mcp.NewToolResultError("At least one thread_id must be provided"), nil
		}

		// Limit to prevent overwhelming requests
		if len(threadIDs) > 20 {
			return mcp.NewToolResultError("Maximum 20 thread_ids allowed per request"), nil
		}

		return gmailServer.FetchEmailBodies(ctx, threadIDs)
	})

	// Add Get OOB Dashboard URL tool
	getDashboardTool := mcp.NewTool("get_oob_dashboard_url",
		mcp.WithDescription("Get the URL for the Out-of-Band approval dashboard where users approve email sends."),
	)

	mcpServer.AddTool(getDashboardTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if approvalSession == nil {
			return mcp.NewToolResultError("No approval session active"), nil
		}
		url := fmt.Sprintf("http://localhost:%d/outbox/%s", oobServerPort, approvalSession.ID)
		return mcp.NewToolResultText(fmt.Sprintf("OOB Dashboard URL: %s\n\nOpen this URL in your browser to approve/reject email sends.", url)), nil
	})

	// Add Send Email ATO tool with OOB approval (Agent Cut-Out Pattern)
	// ATO = Agent Tomfoolery-proof Operation
	sendEmailATOTool := mcp.NewTool("send_email_ato",
		mcp.WithDescription(`Send an email with user approval via mobile push notification.

This tool implements zero-trust email sending:
- Agent handles INPUT (provides email content)
- Approval happens OUT OF BAND on user's phone (agent cannot see or influence)
- Server executes the send only after user approves

How it works:
1. You call this tool with to, subject, body
2. Server creates a draft and sends approval request to user's phone
3. User receives push notification with email preview
4. User taps Approve or Reject on their phone
5. This tool waits for the response (up to 5 minutes)
6. If approved, the email is sent and tool returns success

Returns on success:
- {status: "sent", message: "...", to: "...", subject: "..."}

Returns on rejection or timeout:
- Error message explaining what happened

NOTE: This tool blocks until the user responds on their phone. Tell the user to check their ntfy app.`),
		mcp.WithString("to",
			mcp.Required(),
			mcp.Description("Recipient email address"),
		),
		mcp.WithString("subject",
			mcp.Required(),
			mcp.Description("Email subject line"),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("Email body content"),
		),
		mcp.WithString("thread_id",
			mcp.Description("Thread ID if this is a reply (optional). If provided and a draft exists for this thread, the existing draft will be updated instead of creating a new one."),
		),
	)

	mcpServer.AddTool(sendEmailATOTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		to, err := req.RequireString("to")
		if err != nil {
			return mcp.NewToolResultError("to parameter is required"), nil
		}
		subject, err := req.RequireString("subject")
		if err != nil {
			return mcp.NewToolResultError("subject parameter is required"), nil
		}
		body, err := req.RequireString("body")
		if err != nil {
			return mcp.NewToolResultError("body parameter is required"), nil
		}
		threadID, _ := req.RequireString("thread_id") // optional

		// Create draft internally
		var message gmail.Message
		headers := fmt.Sprintf("To: %s\r\nSubject: %s\r\n", to, subject)

		if threadID != "" {
			message.ThreadId = threadID
			if !strings.HasPrefix(strings.ToLower(subject), "re:") {
				subject = "Re: " + subject
				headers = fmt.Sprintf("To: %s\r\nSubject: %s\r\n", to, subject)
			}
		}

		rawMessage := headers + "\r\n" + body
		message.Raw = base64.URLEncoding.EncodeToString([]byte(rawMessage))

		draft := &gmail.Draft{Message: &message}
		createdDraft, err := gmailServer.service.Users.Drafts.Create(gmailServer.userID, draft).Do()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to create draft: %v", err)), nil
		}

		draftID := createdDraft.Id
		log.Printf("📝 Draft created internally: id=%s to=%s subject=%s", draftID, to, subject)

		// Send to approval daemon for mobile push approval (blocking)
		log.Printf("📱 Sending to approval daemon for mobile push approval...")
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
		log.Printf("✅ Email approved, sending draft...")
		err = gmailServer.SendDraft(draftID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("approved but failed to send: %v", err)), nil
		}

		log.Printf("📧 Email sent successfully: to=%s subject=%s", to, subject)

		resultJSON, _ := json.MarshalIndent(map[string]interface{}{
			"status":  "sent",
			"message": "Email approved and sent successfully",
			"to":      to,
			"subject": subject,
		}, "", "  ")
		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	// Start the server
	if useHTTP {
		log.Printf("Starting Gmail MCP Server in HTTP mode on port %s...", port)
		log.Printf("✅ Server will run persistently at http://localhost:%s", port)
		log.Printf("   OAuth will only be required once at startup!")
		log.Printf("   (Use Ctrl+C to stop the server)")

		// Run Gmail server authentication once at startup
		log.Println("🔐 Authenticating with Gmail (one-time only)...")

		// Test Gmail connection to ensure OAuth is working
		_, err := gmailServer.service.Users.GetProfile(gmailServer.userID).Do()
		if err != nil {
			log.Fatalf("Gmail authentication failed: %v", err)
		}
		log.Println("✅ Gmail authentication successful!")

		// Create HTTP server with CORS support for browser clients
		mux := http.NewServeMux()

		// Add basic info endpoint
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Gmail MCP Server</title></head>
<body>
<h1>📧 Gmail MCP Server</h1>
<p><strong>Status:</strong> Running in HTTP mode on port %s</p>
<p><strong>Cursor Configuration:</strong></p>
<pre>
{
  "mcpServers": {
    "gmail-http": {
      "url": "http://localhost:%s"
    }
  }
}
</pre>
<p><em>Copy the above configuration to your Cursor MCP settings.</em></p>
<h2>Available Tools:</h2>
<ul>
<li>search_threads - Search Gmail with powerful query syntax</li>
<li>create_draft - Create/update email drafts</li>
<li>extract_attachment_by_filename - Extract text from attachments</li>
<li>fetch_email_bodies - Get full email content</li>
<li>get_personal_email_style_guide - Get writing style guide</li>
</ul>
</body>
</html>`, port, port)
		})

		// Add health check endpoint
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")

			status := map[string]interface{}{
				"status":              "healthy",
				"server":              "Gmail MCP Server",
				"version":             "1.0.0",
				"timestamp":           time.Now().Format(time.RFC3339),
				"gmail_authenticated": true,
			}

			json.NewEncoder(w).Encode(status)
		})

		// Add MCP endpoint (simplified HTTP-based MCP)
		mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
			// Enable CORS
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			w.Header().Set("Content-Type", "application/json")

			// Simple implementation - for full MCP support, you'd need
			// to implement the complete JSON-RPC protocol here
			response := map[string]interface{}{
				"jsonrpc": "2.0",
				"result": map[string]interface{}{
					"message":       "Gmail MCP Server HTTP endpoint",
					"note":          "For full MCP support, use stdio mode. HTTP mode is experimental.",
					"stdio_command": os.Args[0], // Path to this binary
				},
			}

			json.NewEncoder(w).Encode(response)
		})

		log.Printf("🌐 HTTP server starting on http://localhost:%s", port)
		log.Printf("📖 View server info: http://localhost:%s", port)
		log.Printf("🔍 Health check: http://localhost:%s/health", port)
		log.Println()
		log.Println("🎯 TO CONNECT CURSOR:")
		log.Printf("   1. For now, use stdio mode (recommended)")
		log.Printf("   2. In Cursor MCP settings, use command: %s", os.Args[0])
		log.Printf("   3. Or wait for full HTTP MCP transport support")

		// Start HTTP server
		httpServer := &http.Server{
			Addr:    ":" + port,
			Handler: mux,
		}

		if err := httpServer.ListenAndServe(); err != nil {
			log.Fatalf("HTTP Server error: %v", err)
		}
	} else {
		log.Println("Starting Gmail MCP Server in stdio mode...")
		log.Println("✅ Server ready! Waiting for MCP client connections via stdio...")
		log.Println("   (Use Ctrl+C to stop the server)")

		if err := server.ServeStdio(mcpServer); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}
}

// ExtractAttachmentByFilename safely extracts text content from an email attachment by filename
// This is more reliable than using attachment IDs which are unstable in Gmail API
func (g *GmailServer) ExtractAttachmentByFilename(ctx context.Context, messageID, filename string) (*mcp.CallToolResult, error) {
	// Get the message to find attachments
	message, err := g.service.Users.Messages.Get(g.userID, messageID).Do()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get message: %v", err)), nil
	}

	// Find all attachments in the message
	allAttachments := extractAttachmentInfo(message)

	// Look for the attachment with matching filename
	var targetAttachment map[string]interface{}
	var attachmentPart *gmail.MessagePart

	for _, attachment := range allAttachments {
		if attachment["filename"] == filename {
			targetAttachment = attachment
			attachmentID := attachment["attachmentId"].(string)
			findAttachmentPart(message.Payload.Parts, attachmentID, &attachmentPart)
			break
		}
	}

	if targetAttachment == nil {
		availableFiles := make([]string, 0, len(allAttachments))
		for _, att := range allAttachments {
			availableFiles = append(availableFiles, att["filename"].(string))
		}
		return mcp.NewToolResultError(fmt.Sprintf("Attachment with filename '%s' not found. Available files: %v", filename, availableFiles)), nil
	}

	if attachmentPart == nil {
		return mcp.NewToolResultError(fmt.Sprintf("Could not find attachment part for filename '%s'", filename)), nil
	}

	// Get the attachment data using the current attachment ID
	attachmentID := targetAttachment["attachmentId"].(string)
	attachment, err := g.service.Users.Messages.Attachments.Get(g.userID, messageID, attachmentID).Do()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get attachment data: %v", err)), nil
	}

	// Decode the attachment data
	data, err := base64.URLEncoding.DecodeString(attachment.Data)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to decode attachment data: %v", err)), nil
	}

	// Extract text based on MIME type
	text, err := extractTextFromBytes(data, attachmentPart.MimeType, attachmentPart.Filename)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to extract text: %v", err)), nil
	}

	result := map[string]interface{}{
		"messageId":    messageID,
		"filename":     filename,
		"attachmentId": attachmentID,
		"mimeType":     attachmentPart.MimeType,
		"textContent":  text,
		"extractedAt":  time.Now().Format(time.RFC3339),
	}

	resultJSON, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(resultJSON)), nil
}

// FetchEmailBodies fetches full email content for multiple threads
func (g *GmailServer) FetchEmailBodies(ctx context.Context, threadIDs []string) (*mcp.CallToolResult, error) {
	var results []map[string]interface{}

	for _, threadID := range threadIDs {
		// Get thread details directly from Gmail API
		threadDetail, err := g.service.Users.Threads.Get(g.userID, threadID).Do()
		if err != nil {
			log.Printf("Warning: Failed to get thread %s: %v", threadID, err)
			continue
		}

		if len(threadDetail.Messages) == 0 {
			continue
		}

		// Extract details from the first message
		firstMessage := threadDetail.Messages[0]
		var subject, from string

		// Extract headers
		for _, header := range firstMessage.Payload.Headers {
			switch header.Name {
			case "Subject":
				subject = header.Value
			case "From":
				from = header.Value
			}
		}

		// Extract full email body content with markdown formatting
		fullBody := extractEmailBody(firstMessage)

		// Limit full body to prevent overwhelming the context (8000 chars = ~2000 tokens)
		if len(fullBody) > 8000 {
			fullBody = fullBody[:8000] + "\n\n[Content truncated - email is longer than 8000 characters]"
		}

		// Collect attachment information from all messages in the thread
		var allAttachments []map[string]interface{}
		for _, message := range threadDetail.Messages {
			attachments := extractAttachmentInfo(message)
			for _, attachment := range attachments {
				// Add message ID to each attachment for reference
				attachment["messageId"] = message.Id
				allAttachments = append(allAttachments, attachment)
			}
		}

		// Get existing drafts for this thread
		existingDrafts, err := g.getThreadDrafts(threadID)
		if err != nil {
			log.Printf("Warning: Failed to get drafts for thread %s: %v", threadID, err)
			existingDrafts = []map[string]interface{}{}
		}

		threadResult := map[string]interface{}{
			"threadId":     threadID,
			"subject":      subject,
			"from":         from,
			"fullBody":     fullBody,
			"messageCount": len(threadDetail.Messages),
		}

		// Only include attachments if there are any
		if len(allAttachments) > 0 {
			threadResult["attachments"] = allAttachments
		}

		// Only include drafts if there are any
		if len(existingDrafts) > 0 {
			threadResult["drafts"] = existingDrafts
		}

		results = append(results, threadResult)
	}

	resultJSON, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal results: %v", err)), nil
	}

	return mcp.NewToolResultText(string(resultJSON)), nil
}
