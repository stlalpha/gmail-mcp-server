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
