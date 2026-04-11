package main

import (
	"log"
	"net/http"
	"os"

	"github.com/hpaliwal8/linq-split/internal/db"
	"github.com/hpaliwal8/linq-split/internal/handlers"
	"github.com/hpaliwal8/linq-split/internal/linq"
	"github.com/hpaliwal8/linq-split/internal/parser"
)

func main() {
	// ── Config from env ──────────────────────────────────────────────
	cfg := &handlers.Config{
		LinqClient: linq.NewClient(
			mustEnv("LINQ_API_TOKEN"),
			mustEnv("LINQ_FROM_NUMBER"),
		),
		WebhookSecret: mustEnv("LINQ_WEBHOOK_SECRET"),
		Parser:        parser.NewClaudeParser(mustEnv("ANTHROPIC_API_KEY")),
	}

	// ── Database ─────────────────────────────────────────────────────
	store, err := db.Open(envOr("DB_PATH", "linq-split.db"))
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()
	cfg.Store = store

	// ── Routes ───────────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", cfg.HandleWebhook)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// ── Start ────────────────────────────────────────────────────────
	port := envOr("PORT", "8080")
	log.Printf("linq-split listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
