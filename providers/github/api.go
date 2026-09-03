package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/providers"
)

// apiError is a non-2xx response from the GitHub API. It never carries the
// token: only the method, path, status and GitHub's own message.
type apiError struct {
	Method     string
	Path       string
	Status     int
	Message    string
	DocURL     string
	RetryAfter time.Duration
	rateLimit  bool
}

func (e *apiError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	switch e.Status {
	case http.StatusUnauthorized:
		return fmt.Sprintf("github api %s %s: 401 %s (token invalid, expired or revoked)", e.Method, e.Path, msg)
	case http.StatusForbidden:
		if e.rateLimit {
			return fmt.Sprintf("github api %s %s: 403 rate limited (%s)", e.Method, e.Path, msg)
		}
		return fmt.Sprintf("github api %s %s: 403 %s (token is missing the \"codespace\" scope?)", e.Method, e.Path, msg)
	default:
		return fmt.Sprintf("github api %s %s: %d %s", e.Method, e.Path, e.Status, msg)
	}
}

// Unwrap maps HTTP status onto the provider-neutral sentinels the core reacts to.
func (e *apiError) Unwrap() error {
	switch {
	case e.Status == http.StatusUnauthorized:
		return providers.ErrAuth
	case e.Status == http.StatusForbidden && !e.rateLimit:
		return providers.ErrAuth
	case e.Status == http.StatusNotFound:
		return providers.ErrNotFound
	case e.Status == http.StatusConflict:
		return providers.ErrConflict
	case e.Status == http.StatusServiceUnavailable:
		return providers.ErrUnavailable
	}
	return nil
}

// Temporary reports whether retrying makes sense.
func (e *apiError) Temporary() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500 || e.rateLimit
}

type apiClient struct {
	http    *http.Client
	base    string
	token   func() string
	timeout time.Duration
	agent   string
}

const maxBody = 8 << 20

func (c *apiClient) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("Authorization", "Bearer "+c.token())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do performs a request with retries for transient failures. out may be nil.
// The returned error is already provider-neutral via apiError.Unwrap.
func (c *apiClient) do(ctx context.Context, method, path string, body, out any) (int, error) {
	const maxAttempts = 3
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			delay := backoffFor(lastErr, attempt)
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(delay):
			}
		}

		attemptCtx, cancel := context.WithTimeout(ctx, c.timeout)
		req, err := c.newRequest(attemptCtx, method, path, body)
		if err != nil {
			cancel()
			return 0, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			cancel()
			// Timeouts and connection failures are worth another try; a
			// cancelled parent context is not.
			if ctx.Err() != nil {
				return 0, fmt.Errorf("github api %s %s: %w", method, path, ctx.Err())
			}
			lastErr = providers.Temporaryf("github api %s %s: %w", method, path, err)
			if attempt == maxAttempts {
				return 0, lastErr
			}
			continue
		}

		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		status := resp.StatusCode
		if readErr != nil {
			cancel()
			lastErr = providers.Temporaryf("github api %s %s: read body: %w", method, path, readErr)
			if attempt == maxAttempts {
				return status, lastErr
			}
			continue
		}
		cancel()

		if status >= 200 && status < 300 {
			if out != nil && len(raw) > 0 {
				if err := json.Unmarshal(raw, out); err != nil {
					return status, fmt.Errorf("github api %s %s: decode response: %w", method, path, err)
				}
			}
			return status, nil
		}

		apiErr := parseAPIError(method, path, status, resp.Header, raw)
		if !apiErr.Temporary() || attempt == maxAttempts {
			return status, apiErr
		}
		lastErr = apiErr
	}
	return 0, lastErr
}

func parseAPIError(method, path string, status int, h http.Header, raw []byte) *apiError {
	e := &apiError{Method: method, Path: path, Status: status}
	var payload struct {
		Message string `json:"message"`
		DocURL  string `json:"documentation_url"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil {
		e.Message = strings.TrimSpace(payload.Message)
		e.DocURL = payload.DocURL
	} else if len(raw) > 0 && len(raw) < 512 {
		e.Message = strings.TrimSpace(string(raw))
	}
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" ||
		strings.Contains(strings.ToLower(e.Message), "rate limit") {
		e.rateLimit = true
	}
	return e
}

func backoffFor(err error, attempt int) time.Duration {
	var ae *apiError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		if ae.RetryAfter > 30*time.Second {
			return 30 * time.Second
		}
		return ae.RetryAfter
	}
	d := time.Duration(1<<uint(attempt-1)) * 500 * time.Millisecond
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}
