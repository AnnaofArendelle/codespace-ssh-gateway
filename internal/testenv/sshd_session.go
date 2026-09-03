package testenv

import (
	"bufio"
	"fmt"
	"strings"

	gossh "golang.org/x/crypto/ssh"
)

type ptyReq struct {
	Term                 string
	Cols, Rows, Wpx, Hpx uint32
	Modes                string
}

type winchReq struct {
	Cols, Rows, Wpx, Hpx uint32
}

type stringReq struct{ Value string }

type exitStatus struct{ Status uint32 }

// handleSession implements just enough of a shell to be observable: it records
// what was requested and answers commands deterministically.
func (s *SSHD) handleSession(ch gossh.Channel, reqs <-chan *gossh.Request) {
	rec := &SSHSession{Env: map[string]string{}}
	s.mu.Lock()
	s.sessions = append(s.sessions, rec)
	s.mu.Unlock()

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var p ptyReq
			if err := gossh.Unmarshal(req.Payload, &p); err == nil {
				rec.PTY, rec.Term, rec.Rows, rec.Cols = true, p.Term, p.Rows, p.Cols
			}
			reply(req, true)
		case "env":
			var kv struct{ Name, Value string }
			if err := gossh.Unmarshal(req.Payload, &kv); err == nil {
				s.mu.Lock()
				rec.Env[kv.Name] = kv.Value
				s.mu.Unlock()
			}
			reply(req, true)
		case "window-change":
			var wc winchReq
			if err := gossh.Unmarshal(req.Payload, &wc); err == nil {
				s.mu.Lock()
				rec.WindowChanges = append(rec.WindowChanges, [2]uint32{wc.Rows, wc.Cols})
				s.mu.Unlock()
			}
			reply(req, true)
		case "signal":
			var sig stringReq
			if err := gossh.Unmarshal(req.Payload, &sig); err == nil {
				s.mu.Lock()
				rec.Signals = append(rec.Signals, sig.Value)
				s.mu.Unlock()
			}
			reply(req, false)
		case "shell":
			rec.Kind = "shell"
			reply(req, true)
			go s.runShell(ch, rec)
		case "exec":
			var m stringReq
			if err := gossh.Unmarshal(req.Payload, &m); err != nil {
				reply(req, false)
				continue
			}
			rec.Kind, rec.Command = "exec", m.Value
			reply(req, true)
			go s.runCommand(ch, m.Value)
		case "subsystem":
			var m stringReq
			if err := gossh.Unmarshal(req.Payload, &m); err != nil {
				reply(req, false)
				continue
			}
			rec.Kind, rec.Command = "subsystem", m.Value
			reply(req, true)
			go s.runSubsystem(ch, m.Value)
		default:
			reply(req, false)
		}
	}
}

func reply(req *gossh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

// runShell is an echo shell: it prints a prompt, echoes each line, and exits on
// "exit". That is enough to prove bytes flow both ways through both hops.
func (s *SSHD) runShell(ch gossh.Channel, rec *SSHSession) {
	fmt.Fprintf(ch, "codespace ready (pty=%v term=%s)\r\n", rec.PTY, rec.Term)
	sc := bufio.NewScanner(ch)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "exit" {
			break
		}
		fmt.Fprintf(ch, "echo: %s\r\n", line)
	}
	sendExit(ch, 0)
}

// runCommand implements a few deterministic commands.
func (s *SSHD) runCommand(ch gossh.Channel, cmd string) {
	switch {
	case strings.HasPrefix(cmd, "echo "):
		fmt.Fprintf(ch, "%s\n", strings.TrimPrefix(cmd, "echo "))
		sendExit(ch, 0)
	case cmd == "whoami":
		fmt.Fprintf(ch, "vscode\n")
		sendExit(ch, 0)
	case cmd == "to-stderr":
		fmt.Fprintf(ch.Stderr(), "this is stderr\n")
		sendExit(ch, 0)
	case strings.HasPrefix(cmd, "exit "):
		code := 0
		_, _ = fmt.Sscanf(cmd, "exit %d", &code)
		sendExit(ch, code)
	case cmd == "env":
		sendExit(ch, 0)
	default:
		fmt.Fprintf(ch.Stderr(), "sh: %s: command not found\n", cmd)
		sendExit(ch, 127)
	}
}

func (s *SSHD) runSubsystem(ch gossh.Channel, name string) {
	fmt.Fprintf(ch, "subsystem:%s\n", name)
	sendExit(ch, 0)
}

func sendExit(ch gossh.Channel, code int) {
	_, _ = ch.SendRequest("exit-status", false, gossh.Marshal(exitStatus{Status: uint32(code)}))
	_ = ch.Close()
}
