package logs

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// Regression: detecting whether an output is a terminal must not take
// ownership of (and later close) the caller's file. Building a second
// *os.File from the raw descriptor attaches a finalizer that closes that
// descriptor when the GC runs, silently killing stdout for the whole process
// some seconds after startup.
func TestSupportsColorDoesNotCloseTheFile(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	supportsColor(f)
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := f.Write([]byte("still open\n")); err != nil {
		t.Fatalf("file was closed underneath us after GC: %v", err)
	}
}