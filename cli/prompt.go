package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// prompter is a small line-based menu driver: numbered choices, defaults on
// enter, and no terminal library, so it works over plain ssh and in tmux.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
	eof bool
}

func newPrompter(out io.Writer) *prompter {
	return &prompter{in: bufio.NewReader(os.Stdin), out: out}
}

// interactive reports whether stdin is a terminal, i.e. whether asking makes sense.
func interactive() bool { return isTerminal(os.Stdin) }

func (p *prompter) line() string {
	text, err := p.in.ReadString('\n')
	if err != nil && text == "" {
		// The terminal went away; stop asking rather than spinning.
		p.eof = true
		return ""
	}
	return strings.TrimSpace(text)
}

// ended reports whether stdin closed, so callers can abort instead of looping.
func (p *prompter) ended() bool { return p.eof }

// ask reads a free-form answer, returning def when the operator just hits enter.
func (p *prompter) ask(question, def string) string {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	answer := p.line()
	if answer == "" {
		return def
	}
	return answer
}

// confirm asks a yes/no question.
func (p *prompter) confirm(question string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		fmt.Fprintf(p.out, "%s [%s]: ", question, hint)
		switch strings.ToLower(p.line()) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		if p.eof {
			return def
		}
		fmt.Fprintln(p.out, "  please answer y or n")
	}
}

// menuItem is one numbered choice.
type menuItem struct {
	Label  string
	Detail string
}

// menu prints a numbered list and returns the chosen index (def is 0-based).
func (p *prompter) menu(title string, items []menuItem, def int) int {
	fmt.Fprintf(p.out, "\n%s\n", title)
	for i, item := range items {
		mark := " "
		if i == def {
			mark = "*"
		}
		fmt.Fprintf(p.out, " %s %d) %s\n", mark, i+1, item.Label)
		if item.Detail != "" {
			fmt.Fprintf(p.out, "       %s\n", item.Detail)
		}
	}
	for {
		fmt.Fprintf(p.out, "choice [%d]: ", def+1)
		answer := p.line()
		if answer == "" {
			return def
		}
		n, err := strconv.Atoi(answer)
		if err == nil && n >= 1 && n <= len(items) {
			return n - 1
		}
		if p.eof {
			return def
		}
		fmt.Fprintf(p.out, "  enter a number between 1 and %d\n", len(items))
	}
}

// secret reads a credential, turning terminal echo off when it can.
func (p *prompter) secret(question string) string {
	restore, quiet := disableEcho()
	if !quiet {
		fmt.Fprintln(p.out, "  (note: your terminal echoes input, so the value will be visible)")
	}
	fmt.Fprintf(p.out, "%s: ", question)
	value := p.line()
	restore()
	if quiet {
		fmt.Fprintln(p.out)
	}
	return value
}

// disableEcho turns off terminal echo using stty, which avoids a terminal
// library dependency. The returned func restores the previous setting.
func disableEcho() (restore func(), ok bool) {
	if !interactive() {
		return func() {}, false
	}
	if _, err := exec.LookPath("stty"); err != nil {
		return func() {}, false
	}
	if err := runSTTY("-echo"); err != nil {
		return func() {}, false
	}
	return func() { _ = runSTTY("echo") }, true
}

func runSTTY(arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
