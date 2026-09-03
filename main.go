// Command gateway exposes a cloud development environment as a plain SSH server.
//
// The provider implementations are linked in here, which is the only place that
// knows which providers exist.
package main

import (
	"os"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/cli"

	_ "github.com/AnnaofArendelle/codespace-ssh-gateway/providers/github"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
