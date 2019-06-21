package main

import (
	"fmt"
	"os"

	"github.com/knz/shakespeare/pkg/cmd"
	"github.com/knz/shakespeare/pkg/crdb/log"
)

func main() {
	if err := cmd.Run(); err != nil {
		cmd.RenderError(log.OrigStderr, err)
		fmt.Fprintln(log.OrigStderr)
		os.Exit(1)
	}
}
