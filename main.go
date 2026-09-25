// Command teledoc is a Telegram-channel document archive with a Papra-style
// web UI: documents posted to a Telegram channel are stored (metadata only)
// in SQLite, auto-tagged by Papra-style tagging rules, and browsable at
// http://localhost:9879 with links back to the original Telegram messages.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"teledoc/internal/rules"
	"teledoc/internal/store"
	"teledoc/internal/telegram"
	"teledoc/internal/web"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// lanIPs returns the non-loopback IPv4 addresses of this machine's active
// network interfaces (e.g. 192.168.x.x), used to print phone-friendly URLs.
func lanIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			if ip4 := ipn.IP.To4(); ip4 != nil {
				// Skip link-local (169.254.x.x autoconfiguration) addresses —
				// no phone can reach them, so they are just log noise.
				if ip4.IsLinkLocalUnicast() {
					continue
				}
				out = append(out, ip4.String())
			}
		}
	}
	return out
}

func main() {
	log.SetFlags(log.LstdFlags)

	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		log.Fatal("BOT_TOKEN env var is required (get one from @BotFather)")
	}
	dbPath := env("DB_PATH", "teledoc.db")
	listenAddr := env("LISTEN_ADDR", ":9879")
	adminPassword := os.Getenv("ADMIN_PASSWORD") // optional

	st, err := store.New(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	engine := rules.New(st, log.New(os.Stdout, "rules ", log.LstdFlags))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the web UI first so the archive is browsable even while the bot
	// is retrying its Telegram connection.
	srv := web.New(st, engine, adminPassword, log.New(os.Stdout, "web ", log.LstdFlags))
	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("web UI listening on http://localhost%s", listenAddr)
		// An address like ":9879" (the default) already binds every interface,
		// so the UI is reachable from other devices on the LAN, e.g. a phone.
		port := listenAddr
		if i := strings.LastIndex(port, ":"); i >= 0 {
			port = port[i+1:]
		}
		if listenAddr == "localhost" || strings.HasPrefix(listenAddr, "localhost:") {
			log.Printf("listening on localhost only; set LISTEN_ADDR=:%s to reach it from your phone", port)
		} else {
			for _, ip := range lanIPs() {
				log.Printf("from your phone (same Wi-Fi): http://%s:%s", ip, port)
			}
		}
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("web server: %v", err)
		}
	}()

	botAPI, err := telegram.New(botToken, st, engine, log.New(os.Stdout, "tg ", log.LstdFlags))
	if err != nil {
		log.Fatalf("telegram bot: %v", err)
	}

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
