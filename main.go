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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"teledoc/internal/logs"
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

// envInt reads an integer env var, returning fallback when unset and logging
// (then using) the fallback when the value is not a valid integer.
func envInt(key string, fallback int, log *log.Logger) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("invalid %s=%q; using default %d (must be a positive whole number of minutes)", key, v, fallback)
		return fallback
	}
	return n
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

	// Temp downloads: where fetched files live and how long a downloaded file
	// stays available. TEMP_DIR is safe to map to a volume (docker-compose
	// maps it to ./temp); only teledoc- prefixed files are ever managed.
	tempDir := env("TEMP_DIR", filepath.Join(os.TempDir(), "teledoc-downloads"))
	tempTTL := time.Duration(envInt("TEMP_FILE_TTL_MINUTES", 10, log.Default())) * time.Minute	// Colored logging: each origin (tg / tg-api / web / rules) prints in
	// yellow, the date+time in purple; colors only on a real terminal, plain
	// text when redirected (docker logs, files). Output runs through an
	// async, drop-safe pipeline: a wedged console must never stall request
	// handlers, the sweeper, or shutdown again (dropped lines are counted
	// and summarized when output recovers).
	loggers, defaultLogger, stopLogs := logs.SetupAsync(os.Stdout)
	defer stopLogs()
	log.SetOutput(defaultLogger.Writer()) // panics, log.Fatal and plain log.Print
	log.SetFlags(0)                       // stamping moved into the loggers
	tgLog := loggers["tg"]
	tgAPILog := loggers["tg-api"]
	webLog := loggers["web"]
	rulesLog := loggers["rules"]

	st, err := store.New(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	engine := rules.New(st, rulesLog)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the web UI first so the archive is browsable even while the bot
	// is retrying its Telegram connection. The lightweight Telegram client
	// used for temp downloads is constructed without a network round trip
	// (its getMe handshake is skipped), so this never blocks either.
	srv := web.New(st, engine, adminPassword, webLog,
		web.TempConfig{Dir: tempDir, TTL: tempTTL},
		telegram.NewClient(botToken, tgAPILog))
	srv.Start(ctx)
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

	botAPI, err := telegram.New(botToken, st, engine, tgLog)
	if err != nil {
		log.Fatalf("telegram bot: %v", err)
	}

	me := botAPI.Me(ctx)
	log.Printf("bot %s polling for channel posts...", me)

	// Blocks until ctx is canceled (Ctrl+C / SIGTERM), then unwinds: the
	// long poll can take up to ~59s to notice and a mid-ingest handler adds
	// a few more seconds, so Run returns late. Shutdown therefore runs in
	// its own goroutine off the canceled context and reports immediately —
	// the user sees the shutdown messages right away instead of a frozen
	// console, and the process exits once Run actually returns (or the
	// hardExit deadline below, whichever comes first).
	go func() {
		<-ctx.Done()
		log.Print("shutting down (Ctrl+C); waiting for Telegram polling to stop...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("web server shutdown: %v", err)
		} else {
			log.Print("web server stopped")
		}
	}()

	// Run blocks until the context unwinds the long poll (up to ~59s) — and
	// under `go run` the toolchain may not wait that long. Wait for polling
	// to end for as long as the server is meant to RUN; the grace deadline
	// applies ONLY after the user has asked to shut down (Ctrl+C / SIGTERM),
	// so a healthy server never self-exits. In-flight state is safe to
	// abandon at that point: SQLite commits per statement and reactions are
	// cosmetic marks.
	runDone := make(chan struct{})
	go func() {
		botAPI.Run(ctx)
		close(runDone)
	}()
	waitForPolling(ctx, runDone, 10*time.Second, func() {
		logs.TryWriteLine(3*time.Second, "bye (Telegram polling still unwinding; exiting)")
		stopLogs()
		os.Exit(0)
	})

	// Run has returned. Say goodbye through the timeout-guarded direct
	// writer: even if the async pipeline or the console is wedged, these
	// lines get a bounded window and shutdown can never hang on them.
	logs.TryWriteLine(3*time.Second, "bye")
}

// waitForPolling blocks until polling ends (runDone closes). If ctx is
// canceled first (Ctrl+C / SIGTERM), the remaining wait is bounded to grace:
// when polling misses that deadline, onDeadline fires (which force-exits).
// While the server is meant to run — ctx still live — this waits forever and
// never exits on its own. Extracted as a pure function so the shutdown
// control flow is unit-testable (a timer bug here once killed healthy
// servers 10s after boot).
func waitForPolling(ctx context.Context, runDone <-chan struct{}, grace time.Duration, onDeadline func()) {
	select {
	case <-runDone:
		return
	case <-ctx.Done():
		select {
		case <-runDone:
		case <-time.After(grace):
			onDeadline()
		}
	}
}
