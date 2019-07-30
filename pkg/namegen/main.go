package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/knz/shakespeare/pkg/cmd"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(1)
	}
	valS := os.Args[1]

	val, err := strconv.ParseInt(valS, 16, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "not a hex number:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "needing %d bits of entropy\n", 4*len(valS))
	fmt.Println("the " + cmd.GenName(val))
}
