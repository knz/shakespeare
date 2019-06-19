package main

import (
	"fmt"
	"os"

	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/pkg/cmd"
)

func main() {
	if err := cmd.Run(); err != nil {
		fmt.Println(err)
		if d := errors.FlattenDetails(err); d != "" {
			fmt.Printf("--\n%s\n", d)
		}
		if d := errors.FlattenHints(err); d != "" {
			fmt.Printf("HINT: %s\n", d)
		}
		os.Exit(1)
	}
}
