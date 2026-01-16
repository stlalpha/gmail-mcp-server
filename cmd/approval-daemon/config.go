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
