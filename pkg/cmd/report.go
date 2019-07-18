package cmd

import (
	"context"
	"os"
	"path/filepath"
)

func (ap *app) writeHtml(ctx context.Context) error {
	fName := filepath.Join(ap.cfg.dataDir, "index.html")
	f, err := os.Create(fName)
	if err != nil {
		return err
	}
	defer f.Close()
	if !ap.cfg.avoidTimeProgress {
		ap.narrate(I, "📄", "index: %s", fName)
	}
	f.WriteString(reportHTML)
	return nil
}
