// Package secret holds values that must never reach logs, terminal output,
// config dumps or error strings.
package secret

import (
	"crypto/subtle"
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

// Redacted is the placeholder printed instead of a secret value.
const Redacted = "***redacted***"

// Value is a string that refuses to render itself. The zero Value is empty.
//
// Value deliberately implements String, GoString, MarshalJSON and MarshalYAML
// so that a secret cannot leak through fmt verbs, slog attributes, JSON
// responses on the control socket, or a re-serialised config file.
type Value struct{ v string }

// New wraps s, trimming surrounding whitespace (tokens pasted from a browser
// or piped from a file routinely carry a trailing newline).
func New(s string) Value { return Value{v: strings.TrimSpace(s)} }

// Reveal returns the plaintext. Every call site should be auditable.
func (s Value) Reveal() string { return s.v }

// Empty reports whether no secret is held.
func (s Value) Empty() bool { return s.v == "" }

// Len returns the plaintext length; safe to log.
func (s Value) Len() int { return len(s.v) }

func (s Value) String() string {
	if s.v == "" {
		return ""
	}
	return Redacted
}

func (s Value) GoString() string { return s.String() }

// Equal compares in constant time.
func (s Value) Equal(other string) bool {
	return subtle.ConstantTimeCompare([]byte(s.v), []byte(other)) == 1
}

func (s Value) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func (s Value) MarshalYAML() (any, error) { return s.String(), nil }

func (s *Value) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*s = New(raw)
	return nil
}

func (s *Value) UnmarshalYAML(n *yaml.Node) error {
	var raw string
	if err := n.Decode(&raw); err != nil {
		return err
	}
	// Guard against a config that was accidentally overwritten with a dump of
	// itself: treat the placeholder as "no secret configured".
	if raw == Redacted {
		raw = ""
	}
	*s = New(raw)
	return nil
}
