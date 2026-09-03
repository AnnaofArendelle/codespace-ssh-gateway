package logging

import (
	"strings"
	"sync"
)

// minRedactLen avoids turning short, low-entropy strings into a global
// search-and-replace that would mangle unrelated log lines.
const minRedactLen = 8

// Redactor holds the set of secret strings that must never appear in output.
// It is safe for concurrent use.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

func NewRedactor() *Redactor { return &Redactor{} }

// Add registers a secret. Short or empty values are ignored.
func (r *Redactor) Add(secrets ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		if len(s) < minRedactLen {
			continue
		}
		if r.has(s) {
			continue
		}
		r.secrets = append(r.secrets, s)
	}
}

func (r *Redactor) has(s string) bool {
	for _, existing := range r.secrets {
		if existing == s {
			return true
		}
	}
	return false
}

// Redact replaces every registered secret in s with a placeholder.
func (r *Redactor) Redact(s string) string {
	if r == nil || s == "" {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, secret := range r.secrets {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}

// RedactBytes is Redact for byte slices captured from subprocess output.
func (r *Redactor) RedactBytes(b []byte) []byte {
	if r == nil || len(b) == 0 {
		return b
	}
	return []byte(r.Redact(string(b)))
}
