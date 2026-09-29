package sessions

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"

	product "github.com/ww1489/seasprak/internal/errors"
)

func newHostShellCommand(ctx context.Context, shell, command string) (*exec.Cmd, string, error) {
	if shell == "" {
		shell = "cmd"
	}
	var name string
	var args []string
	switch shell {
	case "cmd":
		name = "cmd.exe"
	case "powershell", "pwsh":
		name = shell + ".exe"
		args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8\n$__seasprakErrorCount = $Error.Count\n& {\n" + command + "\n}\n$__seasprakSuccess = $?\n$__seasprakExitCode = $LASTEXITCODE\nif (-not $__seasprakSuccess -or $Error.Count -gt $__seasprakErrorCount) { exit 1 }\nif ($null -ne $__seasprakExitCode) { exit $__seasprakExitCode }"}
	default:
		return nil, "", product.NewError(product.CodeInvalidArgument, "unsupported host shell")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, "", product.NewError(product.CodeResourceUnavailable, "host shell is unavailable")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if shell == "cmd" {
		// cmd.exe does not use CommandLineToArgvW quoting. Preserve the user's
		// command string after the shell's own /s /c outer quotes.
		cmd.SysProcAttr.CmdLine = syscall.EscapeArg(path) + " /d /s /c \"" + command + "\""
	}
	// Prefer Windows' process-tree stop. Failure falls back to killing the shell;
	// Terminated is still set only from the actual shell Wait result.
	if stop, err := exec.LookPath("taskkill.exe"); err == nil {
		cmd.Cancel = func() error {
			kill := exec.Command(stop, "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
			kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			if err := kill.Run(); err != nil {
				return cmd.Process.Kill()
			}
			return nil
		}
	}
	return cmd, shell, nil
}
