package codeagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The helper child holds inherited output open until the test releases its TCP
// connection. The parent waits for child readiness before exiting: no sleeps or
// assumptions about PowerShell startup speed establish the inherited-handle case.
func TestP2CommandShellOutputHelper(t *testing.T) {
	args := os.Args
	if len(args) < 5 || args[len(args)-4] != "--shell-output-helper" {
		return
	}
	mode, address, value := args[len(args)-3], args[len(args)-2], args[len(args)-1]
	if mode == "child" {
		control, err := net.Dial("tcp", address)
		if err != nil {
			os.Exit(90)
		}
		fmt.Fprint(os.Stdout, "inherited-output\n")
		if _, err := io.ReadFull(control, make([]byte, 1)); err != nil {
			os.Exit(96)
		}
		ready, err := net.Dial("tcp", value)
		if err != nil {
			os.Exit(91)
		}
		ready.Close()
		io.Copy(io.Discard, control)
		control.Close()
		os.Exit(0)
	}
	ready, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(92)
	}
	child := exec.Command(args[0], "-test.run=^TestP2CommandShellOutputHelper$", "--", "--shell-output-helper", "child", address, ready.Addr().String())
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(93)
	}
	conn, err := ready.Accept()
	if err != nil {
		os.Exit(94)
	}
	conn.Close()
	ready.Close()
	code, err := strconv.Atoi(value)
	if err != nil {
		os.Exit(95)
	}
	os.Exit(code)
}

func inheritedOutputScript(t *testing.T, exitCode int, ready func()) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	child := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			if ready != nil {
				ready()
			}
			_, _ = conn.Write([]byte{1})
			child <- conn
		}
		close(child)
	}()
	t.Cleanup(func() {
		listener.Close()
		if conn := <-child; conn != nil {
			conn.Close() // Release the descendant even when an assertion fails.
		}
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`"%s" -test.run=^TestP2CommandShellOutputHelper$ -- --shell-output-helper parent %s %d`, executable, listener.Addr(), exitCode)
}

func inheritedOutputCommand(t *testing.T, ctx context.Context, exitCode int, ready func()) *exec.Cmd {
	t.Helper()
	cmd, _, err := newHostShellCommand(ctx, "", inheritedOutputScript(t, exitCode, ready))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	cmd.WaitDelay = 50 * time.Millisecond
	return cmd
}

func TestP2CommandShellInheritedOutputPreservesCancellationError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelCalled := make(chan struct{})
	cmd := inheritedOutputCommand(t, ctx, 0, func() {
		cancel()
		select {
		case <-cancelCalled:
		case <-time.After(10 * time.Second):
		}
	})
	cancelErr := errors.New("injected shell cancellation failure")
	cmd.Cancel = func() error {
		close(cancelCalled)
		return cancelErr
	}
	output, incomplete, err := collectHostShellOutput(ctx, cmd)
	t.Logf("cancelled=%v exit=%v incomplete=%v wait=%v injected=%v invalid=%v", ctx.Err(), cmd.ProcessState, incomplete, err, errors.Is(err, cancelErr), errors.Is(err, os.ErrInvalid))
	// Wait may report the cancellation error, a forced process exit, or a
	// subsequent Kill error if exit raced with WaitDelay. Output closure must
	// remain a separate fact instead of replacing any of those with ErrWaitDelay.
	if err == nil || errors.Is(err, exec.ErrWaitDelay) || !errors.Is(ctx.Err(), context.Canceled) || !incomplete || cmd.ProcessState == nil || !strings.Contains(string(output), "inherited-output") {
		t.Fatalf("cancellation hid actual output closure: incomplete=%v state=%v errorType=%T", incomplete, cmd.ProcessState, err)
	}
}

func TestP2CommandShellCompleteOutputIsNotIncomplete(t *testing.T) {
	for _, code := range []int{0, 7} {
		command := shellScript(t, fmt.Sprintf("printf 'complete-output'; exit %d", code), fmt.Sprintf("echo complete-output & exit /b %d", code))
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		cmd, _, err := newHostShellCommand(ctx, "", command)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cmd.WaitDelay = 50 * time.Millisecond
		output, incomplete, err := collectHostShellOutput(ctx, cmd)
		cancel()
		if incomplete || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != code || !strings.Contains(string(output), "complete-output") {
			t.Fatalf("complete output was misclassified: incomplete=%v state=%v errorType=%T", incomplete, cmd.ProcessState, err)
		}
	}
}

func TestP2CommandShellInheritedOutputCannotOutliveTimeout(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := inheritedOutputCommand(t, ctx, code, nil)
			start := time.Now()
			output, incomplete, err := collectHostShellOutput(ctx, cmd)
			t.Logf("exit=%d incomplete=%v timedOut=%v waitType=%T elapsed=%s", cmd.ProcessState.ExitCode(), incomplete, ctx.Err() != nil, err, time.Since(start))
			if cmd.ProcessState.ExitCode() != code || !strings.Contains(string(output), "inherited-output") {
				t.Fatalf("helper did not establish inherited output: exit=%d", cmd.ProcessState.ExitCode())
			}
			var exitErr *exec.ExitError
			if code == 7 && (!errors.As(err, &exitErr) || exitErr.ExitCode() != code) {
				t.Fatalf("process exit error was lost: %T", err)
			}
			if code == 0 && !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("output wait error was lost: %T", err)
			}
			if !incomplete {
				t.Fatal("inherited output truncation was not reported")
			}
			if ctx.Err() != nil {
				t.Fatal("inherited output ignored wait bound")
			}
		})
	}
}
