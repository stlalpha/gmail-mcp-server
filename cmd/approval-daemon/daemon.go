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
