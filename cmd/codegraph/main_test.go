package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/isink17/codegraph/internal/cli"
)

func TestExitCode(t *testing.T) {
	cases := []struct {
		err   error
		code  int
		quiet bool
	}{
		{errors.New("boom"), 1, false},
		{&cli.ExitError{Code: 2, Err: errors.New("stale")}, 2, false},
		{fmt.Errorf("wrapped: %w", &cli.ExitError{Code: 1, Err: errors.New("violations")}), 1, false},
		{&cli.ExitError{Code: 2}, 2, true},
	}
	for _, tc := range cases {
		if code, quiet := exitCode(tc.err); code != tc.code || quiet != tc.quiet {
			t.Errorf("exitCode(%v) = %d, %v; want %d, %v", tc.err, code, quiet, tc.code, tc.quiet)
		}
	}
}
