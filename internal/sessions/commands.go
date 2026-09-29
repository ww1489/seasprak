package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
)

// CommandRequest runs a shell command explicitly submitted by a trusted host.
// Cwd defaults to the bound workspace; relative directories resolve against it.
// The workspace is an initial directory, not a sandbox. Shell defaults to sh on
// Unix and cmd on Windows. Supported names are sh/bash/zsh on Unix and
// cmd/powershell/pwsh on Windows. Timeout defaults to the session's ToolTimeout.
// This entry is not registered as a model tool or exposed through input records.
type CommandRequest struct {
	ExcludeFromContext bool          `json:"excludeFromContext,omitempty"` // !!: retain history without model context
	Command            string        `json:"command"`
	Cwd                string        `json:"cwd,omitempty"`
	Shell              string        `json:"shell,omitempty"`
	Timeout            time.Duration `json:"timeout,omitempty"`
}

// CommandResult describes the actual shell process, even when history saving
// fails. Terminated means Wait observed that process exit, not that its effects
// were rolled back or that detached descendants have stopped. ExitCode is -1
// when no numeric exit status is available. Nonzero exit, timeout and caller
// cancellation are execution results, not API errors.
type CommandResult = agent.HostShellResult

// ExecuteCommand executes once and waits for the actual result. It never
// creates a tool call, claims a ticket, charges Agent budgets, requests approval,
// deduplicates, retries or resumes. Cancelling ctx actively stops the shell;
// cancellation does not return a fabricated terminal result before Wait.
// Errors describe invalid requests, unavailable execution or failed history
// storage. A nonzero result is preserved alongside any history error.
func (s *AgentSession) ExecuteCommand(ctx context.Context, request CommandRequest) (CommandResult, error) {
	var cmd *exec.Cmd
	var runCtx context.Context
	var cancel context.CancelFunc
	var operations tools.Operations
	var binding agent.OutputArtifactBinding
	id := agent.MustID()
	// Once admitted, the mailbox must reply even if the caller cancels; otherwise
	// a registered command could lose its owner and prevent Close from finishing.
	_, err := s.rt.call(context.WithoutCancel(ctx), func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rt.commandBlockedByApproval() {
			return nil, product.NewError(product.CodeStateConflict, "approval wait blocks new host commands until resume or cancellation")
		}
		if strings.TrimSpace(request.Command) == "" || request.Timeout < 0 {
			return nil, product.NewError(product.CodeInvalidArgument, "command and nonnegative timeout are required")
		}
		if rt.opts.Workspace == "" || !filepath.IsAbs(rt.opts.Workspace) {
			return nil, product.NewError(product.CodeInvalidArgument, "an explicit absolute workspace is required")
		}
		if request.Cwd == "" {
			request.Cwd = rt.opts.Workspace
		} else if !filepath.IsAbs(request.Cwd) {
			request.Cwd = filepath.Join(rt.opts.Workspace, request.Cwd)
		}
		if request.Timeout == 0 {
			request.Timeout = rt.opts.Limits.ToolTimeout
		}
		if request.Timeout <= 0 {
			return nil, product.NewError(product.CodeInvalidArgument, "command timeout must be positive")
		}
		runCtx, cancel = context.WithTimeout(ctx, request.Timeout)
		var err error
		cmd, request.Shell, err = newHostShellCommand(runCtx, request.Shell, request.Command)
		if err != nil {
			cancel()
			return nil, err
		}
		cmd.Dir = request.Cwd
		// Bound inherited output handles as well as the shell process itself.
		// Reuse the explicit timeout rather than inventing another time limit.
		cmd.WaitDelay = request.Timeout
		if rt.commands == nil {
			rt.commands = make(map[string]context.CancelFunc)
		}
		rt.commands[id] = cancel
		operations = rt.opts.Operations
		// This is always a host process, even when model tools use a container.
		// CallID identifies only this history entry; no model call is created.
		binding = agent.OutputArtifactBinding{SessionID: rt.opts.SessionID, Environment: "host", CallID: id}
		return nil, nil
	})
	if err != nil {
		return CommandResult{}, err
	}
	defer cancel()
	start := time.Now()
	output, incomplete, runErr := collectHostShellOutput(runCtx, cmd)
	result := CommandResult{Output: string(output), ExitCode: -1, Started: cmd.Process != nil, Terminated: cmd.ProcessState != nil, Duration: time.Since(start)}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	result.OutputIncomplete = incomplete
	result.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
	result.Cancelled = errors.Is(runCtx.Err(), context.Canceled)
	// ExitError is an observed command failure, not an API failure. Never expose
	// raw exec errors, which may contain command text, paths or environment data.
	var exitErr *exec.ExitError
	var executionErr error
	if runErr != nil && !errors.As(runErr, &exitErr) && !result.TimedOut && !result.Cancelled {
		executionErr = product.NewError(product.CodeResourceUnavailable, "host shell could not execute or collect its result")
	}
	// Execution facts above are final before any trusted output callback runs.
	// Share only log preparation/storage with model tools, not their facts,
	// authorization, budgets or two-phase projection protocol.
	prepared, full := tools.PrepareOutputLog(runCtx, operations.OutputRedactor, result.Output)
	result.Output, result.Truncated, result.LogError = prepared.Content, prepared.Truncated, prepared.LogError
	if full != "" {
		result.Artifact, result.LogError = tools.SaveOutputLog(runCtx, operations.Artifacts, agent.OutputArtifactInput{
			Binding: binding, Content: full, MediaType: "text/plain", Name: "shell.log",
		})
	}
	resultSaved := false
	historyErr := s.rt.do(context.Background(), func(rt *runtime) error {
		defer delete(rt.commands, id)
		content, err := json.Marshal(request)
		if err != nil {
			return err
		}
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := rt.manager.SaveHostCommand(context.Background(), agent.AgentMessage{ID: id, Kind: agent.KindCommand, Status: agent.StatusComplete,
			Scope: agent.MessageScope{SessionID: rt.opts.SessionID}, Source: agent.SourceRef{Kind: agent.SourceHuman, Description: "host shell"},
			Command: &agent.CommandMessage{Name: "shell", Content: content, Result: body, ExcludeFromContext: request.ExcludeFromContext}}, request.ExcludeFromContext); err != nil {
			return err
		}
		resultSaved = true
		return rt.flushHostCommands(context.Background())
	})
	if historyErr != nil {
		if result.Artifact.ID != "" && !resultSaved {
			result.Artifact = agent.ArtifactRef{}
			result.LogError = product.CodeStorageUnavailable + ": command log reference was not committed to history"
		}
		return result, historyErr
	}
	return result, executionErr
}

// collectHostShellOutput owns the merged pipe so forced output closure remains
// observable even when Cmd.Wait prefers an ExitError or a cancellation error over
// ErrWaitDelay. cmd.WaitDelay bounds both process waiting and output collection;
// neither shell exit nor pipe closure proves that descendants have stopped.
func collectHostShellOutput(ctx context.Context, cmd *exec.Cmd) ([]byte, bool, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, false, err
	}
	defer reader.Close()
	cmd.Stdout, cmd.Stderr = writer, writer
	var output bytes.Buffer
	var copyErr error
	readDone := make(chan struct{})
	go func() {
		_, copyErr = io.Copy(&output, reader)
		close(readDone)
	}()
	startErr := cmd.Start()
	writer.Close() // Only child handles may keep collection open from here.
	if startErr != nil {
		<-readDone
		return output.Bytes(), false, startErr
	}
	exited := make(chan struct{})
	bounded := make(chan struct{})
	go func() {
		defer close(bounded)
		select {
		case <-readDone:
			return
		case <-ctx.Done():
		case <-exited:
		}
		timer := time.NewTimer(cmd.WaitDelay)
		defer timer.Stop()
		select {
		case <-readDone:
		case <-timer.C:
			reader.Close()
		}
	}()
	runErr := cmd.Wait()
	close(exited)
	<-readDone
	<-bounded
	incomplete := errors.Is(copyErr, os.ErrClosed)
	if runErr == nil {
		if incomplete {
			runErr = exec.ErrWaitDelay
		} else {
			runErr = copyErr
		}
	}
	return output.Bytes(), incomplete, runErr
}
