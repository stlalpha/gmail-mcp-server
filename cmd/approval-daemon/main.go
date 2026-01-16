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
	log.Printf("   ntfy topic: %s", config.NtfyTopic)
	log.Printf("   Socket: %s", getSocketPath())
	log.Println("═══════════════════════════════════════════════════════════════")

	// Run socket server (blocking)
	socketServer.run()
	return nil
}
