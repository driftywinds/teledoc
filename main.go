// Command tgarchive is a Telegram-channel document archive with a Papra-style
// web UI: documents posted to a Telegram channel are stored (metadata only)
// in SQLite, auto-tagged by Papra-style tagging rules, and browsable at
// http://localhost:8080 with links back to the original Telegram messages.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tgarchive/internal/rules"
	"tgarchive/internal/store"
	"tgarchive/internal/telegram"
	"tgarchive/internal/web"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log.SetFlags(log.LstdFlags)

	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		log.Fatal("BOT_TOKEN env var is required (get one from @BotFather)")
	}
	dbPath := env("DB_PATH", "tgarchive.db")
	listenAddr := env("LISTEN_ADDR", ":8080")
	adminPassword := os.Getenv("ADMIN_PASSWORD") // optional

	st, err := store.New(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	engine := rules.New(st, log.New(os.Stdout, "rules ", log.LstdFlags))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	botAPI, err := telegram.New(botToken, st, engine, log.New(os.Stdout, "tg ", log.LstdFlags))
	if err != nil {
		log.Fatalf("telegram bot: %v", err)
	}

	srv := web.New(st, engine, adminPassword, log.New(os.Stdout, "web ", log.LstdFlags))
	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("web UI listening on http://localhost%s", listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("web server: %v", err)
		}
	}()

	me := botAPI.Me(ctx)
	log.Printf("bot %s polling for channel posts...", me)

	// Blocks until ctx is canceled (Ctrl+C / SIGTERM).
	botAPI.Run(ctx)

	log.Println("shutting down web server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	log.Println("bye")
}
