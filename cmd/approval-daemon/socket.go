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
