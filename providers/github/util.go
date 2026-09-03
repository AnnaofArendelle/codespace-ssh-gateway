package github

import (
	"os/exec"
	"strings"
	"sync"
)

// safeBuffer is a bounded, concurrency-safe sink for a child process's stderr.
// It keeps the first N bytes, which is where the useful error message is.
type safeBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func newSafeBuffer(limit int) *safeBuffer { return &safeBuffer{limit: limit} }

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
		}
	}
	return len(p), nil
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Tail returns the last n non-empty lines, for error messages.
func (b *safeBuffer) Tail(n int) string {
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	out := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			out = append([]string{line}, out...)
		}
	}
	return strings.Join(out, "; ")
}

// procWatch reaps a child process once and lets several goroutines wait for it.
type procWatch struct {
	done chan struct{}
	err  error
}

func watchProcess(cmd *exec.Cmd) *procWatch {
	w := &procWatch{done: make(chan struct{})}
	go func() {
		w.err = cmd.Wait()
		close(w.done)
	}()
	return w
}

func termOr(term string) string {
	if term == "" {
		return "xterm-256color"
	}
	return term
}

func dimOr(v, fallback uint16) int {
	if v == 0 {
		return int(fallback)
	}
	return int(v)
}
