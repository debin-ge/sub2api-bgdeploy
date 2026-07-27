// Command bgdeploy is the sub2api blue-green deployment CLI.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/debin-ge/sub2api-bgdeploy/internal/cli"
)

func main() {
	if err := cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}
