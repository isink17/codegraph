package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/isink17/codegraph/internal/cli"
)

func main() {
	if err := cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		code, quiet := exitCode(err)
		if !quiet {
			fmt.Fprintln(os.Stderr, err.Error())
		}
		os.Exit(code)
	}
}

// exitCode maps a command error to the process exit status. Commands exit 1 on
// any error unless they return a cli.ExitError naming another code; one with a
// nil Err has already reported itself and exits without a message.
func exitCode(err error) (code int, quiet bool) {
	var exitErr *cli.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code, exitErr.Err == nil
	}
	return 1, false
}
