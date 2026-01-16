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
