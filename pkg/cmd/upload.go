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
	target := cfg.uploadURL + "/" + filepath.Base(cfg.dataDir)
	var cmd *exec.Cmd
	switch {
	case strings.HasPrefix(cfg.uploadURL, "s3:"):
		cmd = exec.CommandContext(ctx, "aws", "cp", "--recursive", cfg.dataDir, target)
	case strings.HasPrefix(target, "gs:"):
		cmd = exec.CommandContext(ctx, "gsutil", "-m", "cp", "-r", cfg.dataDir, target)
	case strings.HasPrefix(target, "scp://"):
		cmd = exec.CommandContext(ctx, "scp", "-r", cfg.dataDir, target)
	default:
		return errors.Newf("unsupported URL scheme: %q", target)
	}
	ap.narrate(I, "⬆️", "uploading with: %s", strings.Join(cmd.Args, " "))
	res, err := cmd.CombinedOutput()
	log.Infof(ctx, "upload:\n%s\n-- %v / %s", string(res), err, cmd.ProcessState)
	if err != nil {
		ap.narrate(E, "😿", "upload error:\n%s", string(res))
	}
	return err
}
