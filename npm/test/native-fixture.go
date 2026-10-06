package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "--version":
		fmt.Printf("codegraph %s\n", os.Getenv("CODEGRAPH_TEST_VERSION"))
	case "fail":
		os.Exit(23)
	case "stdin":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "args":
		for _, arg := range args[1:] {
			fmt.Printf("<%s>\n", arg)
		}
	}
}
