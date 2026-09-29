package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
)

// HostShellResult contains the observed host process facts. It is shared by
// sessions and message projection so history is decoded with the same contract.
type HostShellResult struct {
	OutputIncomplete bool          `json:"outputIncomplete,omitempty"`
	Output           string        `json:"output"`
	Truncated        bool          `json:"truncated,omitempty"`
	Artifact         ArtifactRef   `json:"artifact,omitempty"`
	LogError         string        `json:"logError,omitempty"`
	ExitCode         int           `json:"exitCode"`
	Started          bool          `json:"started"`
	Terminated       bool          `json:"terminated"`
	TimedOut         bool          `json:"timedOut"`
	Cancelled        bool          `json:"cancelled"`
	Duration         time.Duration `json:"duration"`
}

// DecodeHostShellCommand validates stored facts, never inferring execution from
// missing fields. Optional fields in older shell records retain zero defaults.
func DecodeHostShellCommand(c CommandMessage) (string, HostShellResult, error) {
	var request struct {
		Command string `json:"command"`
	}
	var result HostShellResult
	var required struct {
		Output     *string `json:"output"`
		ExitCode   *int    `json:"exitCode"`
		Started    *bool   `json:"started"`
		Terminated *bool   `json:"terminated"`
	}
	if c.Name != "shell" || json.Unmarshal(c.Content, &request) != nil || strings.TrimSpace(request.Command) == "" || json.Unmarshal(c.Result, &result) != nil || json.Unmarshal(c.Result, &required) != nil || required.Output == nil || required.ExitCode == nil || required.Started == nil || required.Terminated == nil || result.Duration < 0 || result.ExitCode < -1 || (result.Terminated && !result.Started) || (!result.Terminated && result.ExitCode != -1) || (result.Cancelled && result.TimedOut) {
		return "", HostShellResult{}, product.NewError(product.CodeInvalidArgument, "invalid host shell request or execution result")
	}
	return request.Command, result, nil
}

func hostShellText(c CommandMessage) (string, error) {
	command, result, err := DecodeHostShellCommand(c)
	if err != nil {
		return "", err
	}
	suffix := hostShellSuffix(result, false)
	text := formatHostShellText(command, result.Output, suffix)
	if hostShellTextFits(text) {
		return text, nil
	}
	limit := "byte limit"
	if strings.Count(text, "\n")+1 > hostShellModelLines {
		limit = "line limit"
		if len(text) > processModelContentBytes {
			limit = "byte and line limits"
		}
	}
	note := "\n\n[Shell message truncated for model context (" + limit + "). Command/output previews retain head/tail fragments; partial lines may be shown. Original command/result remain in session history.]"
	suffix += note
	// References are indivisible identities. If even the minimum preview cannot
	// coexist with the complete reference, omit only its model representation.
	if !hostShellTextFits(formatHostShellText("[truncated]", "[truncated]", suffix)) {
		suffix = hostShellSuffix(result, true) + note
	}
	bytes, lines := processModelContentBytes, hostShellModelLines
	for !hostShellTextFits(text) {
		if len(text) > processModelContentBytes {
			bytes = max(0, bytes-max(1, bytes/8))
		}
		if strings.Count(text, "\n")+1 > hostShellModelLines {
			lines = max(1, lines-max(1, lines/8))
		}
		text = formatHostShellText(hostShellModelPreview(command, bytes, lines), hostShellModelPreview(result.Output, bytes, lines), suffix)
	}
	return text, nil
}

// Shell head/tail previews use the delivery plan's 2,000-line/50-KiB limits.
// Count the final formatted model text, including fences, status and references.
const hostShellModelLines = 2000

func hostShellTextFits(text string) bool {
	return len(text) <= processModelContentBytes && strings.Count(text, "\n")+1 <= hostShellModelLines
}

func formatHostShellText(command, output, suffix string) string {
	text := "Ran `" + command + "`\n"
	if output != "" {
		text += "```\n" + output + "\n```"
	} else {
		text += "(no output)"
	}
	return text + suffix
}

func hostShellModelPreview(text string, bytes, lines int) string {
	if strings.Count(text, "\n")+1 > lines {
		if lines < 3 {
			text = "[truncated]"
		} else {
			parts := strings.Split(text, "\n")
			head, tail := lines/2, (lines-1)/2
			text = strings.Join(parts[:head], "\n") + "\n[truncated]\n" + strings.Join(parts[len(parts)-tail:], "\n")
		}
	}
	if len(text) > bytes {
		text = processModelPreview(text, bytes)
		if text == "" {
			text = "[truncated]"
		}
	}
	return text
}

func hostShellSuffix(result HostShellResult, omitReference bool) string {
	var text string
	if result.Cancelled {
		text += "\n\n(command cancelled)"
	} else if result.TimedOut {
		text += "\n\n(command timed out)"
	} else if result.ExitCode != 0 {
		text += fmt.Sprintf("\n\nCommand exited with code %d", result.ExitCode)
	}
	if result.Truncated {
		if result.Artifact.ID != "" && result.Artifact.Available {
			if omitReference {
				text += "\n\n[Output truncated. Full output artifact reference omitted from model display due to size limits.]"
			} else {
				text += "\n\n[Output truncated. Full output artifact: " + result.Artifact.ID + "]"
			}
		} else {
			text += "\n\n[Output truncated. Full output unavailable.]"
		}
	}
	if result.OutputIncomplete {
		text += "\n\n[Output collection incomplete.]"
	}
	return text
}
