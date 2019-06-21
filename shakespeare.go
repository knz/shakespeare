package main

import (
	"fmt"
	"os"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/cmd"
)

func main() {
	if err := cmd.Run(); err != nil {
		fmt.Println("error:", err)
		if bufs := errors.GetContextTags(err); len(bufs) > 0 {
			var buf *logtags.Buffer
			for _, b := range bufs {
				buf = buf.Merge(b)
			}
			fmt.Printf("--\n(context of error: %s)\n", buf)
		}
		if d := errors.FlattenDetails(err); d != "" {
			fmt.Printf("--\n%s\n", d)
		}
		if d := errors.FlattenHints(err); d != "" {
			fmt.Printf("HINT: %s\n", d)
		}
		os.Exit(1)
	}
}
