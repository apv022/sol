// Command sol is the Sol daemon, its CLI, and the first-party apps in one binary.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "sol: not built yet (see design/PLAN.md)")
	os.Exit(1)
}
