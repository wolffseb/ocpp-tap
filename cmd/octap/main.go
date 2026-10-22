// Command octap is the ocpp-tap binary: an OCPP 1.6-J forwarder with a
// read-only tap. Only `octap version` exists so far; the forwarder arrives
// with SP1 (see docs/PRD.md).
package main

import (
	"fmt"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Println("octap", version)
		return 0
	}
	fmt.Fprintln(os.Stderr, "octap: not implemented yet (SP1); only `octap version` works")
	return 2
}
