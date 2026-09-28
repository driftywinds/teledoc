package logs

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// With colors off, output is byte-identical to the historical format.
func TestWriterPlainPassThrough(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, false)
	_, _ = w.Write([]byte("web 2026/09/27 19:54:46 download triggered for document 42\n"))
	got := buf.String()
	want := "web 2026/09/27 19:54:46 download triggered for document 42\n"
	if got != want {
		t.Fatalf("plain output = %q, want %q", got, want)
	}
}

// With colors on, the origin is yellow and the stamp purple, with resets in
// the right places.
func TestWriterColored(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, true)
	_, _ = w.Write([]byte("tg 2026/09/27 19:54:46 archived \"x.pdf\"\n"))
	want := "\x1b[93mtg\x1b[0m \x1b[95m2026/09/27 19:54:46\x1b[0m archived \"x.pdf\"\n"
	if got := buf.String(); got != want {
		t.Fatalf("colored output = %q, want %q", got, want)
	}
}

// Lines that do not start with a known origin pass through verbatim (e.g.
// panic dumps from a dependency), never mangled or lost.
func TestWriterUnknownOriginVerbatim(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, true)
	line := "goroutine 1 [running]:\nruntime.main()\n"
	_, _ = w.Write([]byte(line))
	if got := buf.String(); got != line {
		t.Fatalf("unknown-shape output = %q, want verbatim %q", got, line)
	}
}

// A stamp-less line from a known origin passes through verbatim too (the
// colored re-shuffle requires the full shape).
func TestWriterMissingStampVerbatim(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, true)
	_, _ = w.Write([]byte("web something without a stamp\n"))
	if got := buf.String(); got != "web something without a stamp\n" {
		t.Fatalf("stamp-less output = %q", got)
	}
}

// The default logger stamps with the app origin and the same layout.
func TestSetupDefaultLogger(t *testing.T) {
	var buf bytes.Buffer
	// Force colors off for a deterministic assertion; detection is covered
	// separately below.
	saved := os.Getenv("NO_COLOR")
	os.Setenv("NO_COLOR", "1")
	defer func() {
		if saved == "" {
			os.Unsetenv("NO_COLOR")
		} else {
			os.Setenv("NO_COLOR", saved)
		}
	}()

	loggers, def := Setup(&buf)
	if loggers["web"] == nil || loggers["tg"] == nil || loggers["tg-api"] == nil || loggers["rules"] == nil {
		t.Fatalf("Setup did not build all origin loggers: %v", loggers)
	}
	def.Print("web UI listening on http://localhost:9879")

	out := buf.String()
	if !strings.HasPrefix(out, "app ") {
		t.Fatalf("default logger output = %q, want app origin", out)
	}
	if !strings.Contains(out, "web UI listening") {
		t.Fatalf("default logger output = %q, message lost", out)
	}
	// The stamp must parse: app YYYY/MM/DD HH:MM:SS message
	fields := strings.SplitN(strings.TrimSpace(out), " ", 4)
	if len(fields) != 4 || len(fields[1]) != 10 || len(fields[2]) != 8 {
		t.Fatalf("default logger stamp malformed: %q", out)
	}
}

// Multi-line payloads keep their interior newlines after stamping.
func TestOriginLoggerMultiLine(t *testing.T) {
	var buf bytes.Buffer
	saved := os.Getenv("NO_COLOR")
	os.Setenv("NO_COLOR", "1")
	defer func() {
		if saved == "" {
			os.Unsetenv("NO_COLOR")
		} else {
			os.Setenv("NO_COLOR", saved)
		}
	}()
	loggers, _ := Setup(&buf)
	loggers["web"].Print("line one\nline two")

	if !strings.Contains(buf.String(), "line one\nline two") {
		t.Fatalf("multi-line payload mangled: %q", buf.String())
	}
}

// supportsColor honors NO_COLOR and the non-terminal case.
func TestSupportsColor(t *testing.T) {
	saved, had := os.LookupEnv("NO_COLOR")
	os.Setenv("NO_COLOR", "1")
	if supportsColor(os.Stdout) {
		t.Error("NO_COLOR must disable colors")
	}
	os.Unsetenv("NO_COLOR")

	if supportsColor(&bytes.Buffer{}) {
		t.Error("a bytes.Buffer is not a terminal; colors must be off")
	}

	if had {
		os.Setenv("NO_COLOR", saved)
	} else {
		os.Unsetenv("NO_COLOR")
	}
}
