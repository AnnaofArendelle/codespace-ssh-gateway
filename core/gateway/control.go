package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// controlServer is a tiny local API on a unix socket (mode 0600) so that
// `gateway status` and `gateway stop` can talk to a running gateway. It is not
// reachable from the network.
type controlServer struct {
	g    *Gateway
	path string
	ln   net.Listener
	srv  *http.Server
}

func newControlServer(g *Gateway, path string) (*controlServer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// A live socket means another gateway is running; a dead one is leftovers.
	if c, err := net.DialTimeout("unix", path, 300*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("another gateway is already using %s", path)
	}
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}

	cs := &controlServer{g: g, path: path, ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", cs.handleStatus)
	mux.HandleFunc("POST /stop", cs.handleStop)
	mux.HandleFunc("POST /environment/stop", cs.handleStopEnvironment)
	cs.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	g.log.Info("control socket ready", slog.String("path", path))
	return cs, nil
}

func (cs *controlServer) serve() {
	if err := cs.srv.Serve(cs.ln); err != nil && err != http.ErrServerClosed {
		cs.g.log.Debug("control socket closed", slog.Any("error", err))
	}
}

func (cs *controlServer) close() {
	if cs.srv != nil {
		_ = cs.srv.Close()
	}
	_ = os.Remove(cs.path)
}

func (cs *controlServer) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, cs.g.Status())
}

func (cs *controlServer) handleStop(w http.ResponseWriter, _ *http.Request) {
	cs.g.log.Info("stop requested over the control socket")
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
	go func() {
		time.Sleep(100 * time.Millisecond)
		cs.g.Shutdown()
	}()
}

// handleStopEnvironment stops one environment on request. This is an explicit
// operator action, distinct from any idle handling.
func (cs *controlServer) handleStopEnvironment(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = cs.g.prov.DefaultEnvironment()
	}
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no environment given"})
		return
	}
	if err := cs.g.life.Stop(r.Context(), name); err != nil {
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": cs.g.redact.Redact(err.Error())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped", "environment": name})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// PIDFile is where a running gateway records its pid.
func PIDFile(stateDir string) string { return filepath.Join(stateDir, "gateway.pid") }

// writePIDFile records the current pid; the returned func removes it.
func writePIDFile(stateDir string) (func(), error) {
	path := PIDFile(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}
