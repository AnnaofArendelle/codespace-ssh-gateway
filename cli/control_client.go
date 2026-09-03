package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/gateway"
)

// controlClient talks to a running gateway over its unix control socket.
type controlClient struct {
	path string
	http *http.Client
}

func newControlClient(path string) *controlClient {
	return &controlClient{
		path: path,
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", path)
				},
			},
		},
	}
}

// alive reports whether a gateway is listening on the socket.
func (c *controlClient) alive() bool {
	conn, err := net.DialTimeout("unix", c.path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (c *controlClient) do(method, path string, out any) error {
	req, err := http.NewRequest(method, "http://gateway"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control socket %s: %w", c.path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &payload)
		if payload.Error != "" {
			return fmt.Errorf("gateway: %s", payload.Error)
		}
		return fmt.Errorf("gateway returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *controlClient) status() (*gateway.Status, error) {
	var st gateway.Status
	if err := c.do("GET", "/status", &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (c *controlClient) stop() error { return c.do("POST", "/stop", nil) }

func (c *controlClient) stopEnvironment(name string) error {
	return c.do("POST", "/environment/stop?name="+url.QueryEscape(name), nil)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
