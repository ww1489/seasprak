//go:build !windows

package sessions

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"

	product "github.com/ww1489/seasprak/internal/errors"
)

func newHostShellCommand(ctx context.Context, shell, command string) (*exec.Cmd, string, error) {
	if shell == "" {
		shell = "sh"
	}
	if shell != "sh" && shell != "bash" && shell != "zsh" {
		return nil, "", product.NewError(product.CodeInvalidArgument, "unsupported host shell")
	}
	path, err := exec.LookPath(shell)
	if err != nil {
		return nil, "", product.NewError(product.CodeResourceUnavailable, "host shell is unavailable")
	}
	cmd := exec.CommandContext(ctx, path, "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return cmd, shell, nil
}
