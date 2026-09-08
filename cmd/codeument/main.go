// Command codeument is the CLI entry point for the codeument client and relay.
package main

import (
	"fmt"
	"os"

	"github.com/Tzurrr/codeument/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "codeument:", err)
		os.Exit(1)
	}
}
