package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	port := flag.Int("port", 8888, "HTTP listen port")
	masterSecret := flag.String("master-secret", "", "HMAC master secret (must match coordinator)")
	dbPath := flag.String("db", "portal.db", "SQLite database path")
	templatesDir := flag.String("templates", "", "path to templates directory (auto-detected if empty)")
	swarmAPI := flag.String("swarm-api", "http://localhost:14690", "SwarmAPI base URL (coordinator HTTP endpoint)")
	flag.Parse()

	if *masterSecret == "" {
		if env := os.Getenv("IOSWARM_MASTER_SECRET"); env != "" {
			*masterSecret = env
		} else {
			fmt.Fprintf(os.Stderr, "error: --master-secret is required\n")
			os.Exit(1)
		}
	}

	// Auto-detect templates dir
	if *templatesDir == "" {
		exe, _ := os.Executable()
		candidates := []string{
			"templates",
			filepath.Join(filepath.Dir(exe), "templates"),
		}
		for _, c := range candidates {
			if info, err := os.Stat(c); err == nil && info.IsDir() {
				*templatesDir = c
				break
			}
		}
		if *templatesDir == "" {
			*templatesDir = "templates"
		}
	}

	store, err := NewStore(*dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	app := NewApp(store, *masterSecret, *templatesDir, *swarmAPI)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("IOSwarm Portal listening on %s", addr)
	log.Printf("  master-secret: %s...%s", (*masterSecret)[:4], (*masterSecret)[len(*masterSecret)-4:])
	log.Printf("  database: %s", *dbPath)

	if err := http.ListenAndServe(addr, app.Routes()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
