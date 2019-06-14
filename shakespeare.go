package main

import (
	"fmt"
	"os"

	"github.com/knz/shakespeare/pkg/cmd"
)

func main() {
	if err := cmd.Run(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
