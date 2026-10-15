// Command limits-explain shows every limit binding a Linux process.
package main

import (
	"os"

	"github.com/mabunemeh/limits-explain/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
