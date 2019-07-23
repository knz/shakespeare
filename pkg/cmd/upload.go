package cmd

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/pkg/crdb/log"
)

func (ap *app) tryUpload(ctx context.Context) error {
	cfg := ap.cfg
	if cfg.uploadURL == "" {
		return nil
	}

	var args []string
	switch {
	case strings.HasPrefix(cfg.uploadURL, "s3:"):
		args = []string{"aws", "cp", "--recursive"}
	case strings.HasPrefix(cfg.uploadURL, "gs:"):
		args = []string{"gsutil", "-m", "cp", "-r"}
	case strings.HasPrefix(cfg.uploadURL, "scp://"):
		args = []string{"scp", "-r"}
	default:
		return errors.Newf("unsupported URL scheme: %q", cfg.uploadURL)
	}
	target := cfg.uploadURL + "/" + filepath.Base(cfg.dataDir)
	args = append(args, cfg.dataDir, target)

	redirect := " >" + filepath.Join(cfg.dataDir, "upload.log") + " 2>&1"
	cmd := exec.CommandContext(ctx, cfg.shellPath, "-c", strings.Join(args, " ")+redirect)
	ap.narrate(I, "⬆️", "uploading with: %s", strings.Join(cmd.Args, " "))
	res, err := cmd.CombinedOutput()
	log.Infof(ctx, "upload:\n%s\n-- %v / %s", string(res), err, cmd.ProcessState)
	if err != nil {
		ap.narrate(E, "😿", "upload error, check upload.log")
	}
	return err
}
