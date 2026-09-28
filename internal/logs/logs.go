// Package logs renders log entries with a colored origin and timestamp so a
// glance separates who logged (tg / tg-api / web / rules) from when and what.
//
// Entry layout (origin first, matching the historical format):
//
//	web 2026/09/27 19:54:46 download triggered ...
//
// with the origin word in yellow and the date+time in purple (magenta).
// Colors are emitted only when the destination can render them: a real
// terminal with ANSI support. Redirects, pipes, docker log capture and
// NO_COLOR all get plain text, so log files stay clean.
package logs

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// ANSI SGR codes used. Purple is rendered as bright magenta (95), which
// reads as purple on every mainstream terminal palette.
const (
	ansiReset  = "\x1b[0m"
	ansiOrigin = "\x1b[93m" // bright yellow: who logged
	ansiTime   = "\x1b[95m" // bright magenta: when
)

// Origins this program uses; the Setup map is built from it.
var Origins = []string{"tg", "tg-api", "web", "rules", "app"}

// Writer is an io.Writer receiving fully stamped lines of the shape
// "<origin> <date> <time> <message>" and re-emitting them with the origin in
// yellow and the date+time in purple. When color is disabled it passes the
// line through untouched — byte-identical to the historical log output.
type Writer struct {
	mu    sync.Mutex
	w     io.Writer
	color bool
}

// NewWriter wraps w. When color is false the line is passed through
// unchanged.
func NewWriter(w io.Writer, color bool) *Writer {
	return &Writer{w: w, color: color}
}

// ForOutput returns a writer for out: colors enabled only when out is a
// terminal capable of ANSI (and NO_COLOR is not set — see
// https://no-color.org). FORCE_COLOR=1 forces colors on for piped output.
func ForOutput(out io.Writer) *Writer {
	return NewWriter(out, supportsColor(out))
}

// supportsColor reports whether ANSI SGR sequences should be emitted to out.
func supportsColor(out io.Writer) bool {
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	if os.Getenv("FORCE_COLOR") == "1" {
		return true
	}
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	return isTerminal(f)
}

// Write implements io.Writer. Log lines arrive with a trailing newline and
// the shape "<origin> <date> <time> <message>" (stamped by originLogger).
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.color {
		return w.w.Write(p) // pass through untouched
	}
	line := strings.TrimRight(string(p), "\n")
	origin, rest, restOK := splitOrigin(line)
	if !restOK {
		// Unrecognized shape: emit as-is rather than mangling it.
		return w.w.Write(p)
	}
	stamp, msg, stampOK := splitStamp(rest)
	if !stampOK {
		return w.w.Write(p)
	}
	out := fmt.Sprintf("%s%s%s %s%s%s %s\n", ansiOrigin, origin, ansiReset, ansiTime, stamp, ansiReset, msg)
	if _, err := io.WriteString(w.w, out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// splitOrigin splits "<origin> rest" when origin is one this program uses;
// ok is false when the line does not start with a known origin.
func splitOrigin(line string) (origin, rest string, ok bool) {
	i := strings.IndexByte(line, ' ')
	if i <= 0 {
		return "", "", false
	}
	candidate := line[:i]
	for _, known := range Origins {
		if candidate == known {
			return candidate, line[i+1:], true
		}
	}
	return "", "", false
}

// splitStamp splits "2006/01/02 15:04:05 message" into the stamp and the
// message. ok is false for anything unexpected (a multi-line panic dump,
// output from a dependency that bypassed our loggers) so Write can emit it
// verbatim instead of mangling it.
func splitStamp(rest string) (stamp, msg string, ok bool) {
	parts := strings.SplitN(rest, " ", 3)
	if len(parts) >= 3 && looksLikeStamp(parts[0]) && looksLikeClock(parts[1]) {
		return parts[0] + " " + parts[1], parts[2], true
	}
	return "", "", false
}

func looksLikeStamp(s string) bool {
	if len(s) != len("2006/01/02") {
		return false
	}
	for i, c := range s {
		switch i {
		case 4, 7:
			if c != '/' {
				return false
			}
		default:
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

func looksLikeClock(s string) bool {
	if len(s) != len("15:04:05") {
		return false
	}
	for i, c := range s {
		switch i {
		case 2, 5:
			if c != ':' {
				return false
			}
		default:
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// Setup builds the process-wide loggers writing colored entries to out: one
// *log.Logger per known origin, plus the returned default logger for plain
// log.Print/Fatal calls (stamp alone, no origin). Callers pass the result to
// the components; wire the default into log.SetOutput in main so panics and
// fatals share the same format.
func Setup(out io.Writer) (loggers map[string]*log.Logger, defaultLogger *log.Logger) {
	w := ForOutput(out)
	loggers = map[string]*log.Logger{}
	for _, origin := range Origins {
		loggers[origin] = newOriginLogger(w, origin)
	}
	defaultLogger = newOriginLogger(w, "app")
	return loggers, defaultLogger
}

// originLogger stamps each Write with "<origin> <date> <time> " so the
// colored Writer can re-shuffle it into columns. It replaces log.New(prefix)
// with LstdFlags so the stamp exists in exactly one canonical format.
type originLogger struct {
	w      io.Writer
	origin string
	now    func() time.Time
	mu     sync.Mutex
}

func newOriginLogger(w io.Writer, origin string) *log.Logger {
	return log.New(&originLogger{w: w, origin: origin, now: time.Now}, "", 0)
}

func (o *originLogger) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	stamp := o.now().Format("2006/01/02 15:04:05")
	// Keep multi-line payloads intact; the stamp prefixes the first line.
	body := strings.TrimRight(string(p), "\n")
	_, err := fmt.Fprintf(o.w, "%s %s %s\n", o.origin, stamp, body)
	return len(p), err
}