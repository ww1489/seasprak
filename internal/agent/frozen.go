package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// FrozenExecution is the committed, immutable description of one tool start.
// References, rather than secret values, are retained in the journal.
type FrozenExecution struct {
	ID                    string              `json:"id"`
	CallID                string              `json:"callId"`
	Hash                  string              `json:"hash"`
	Scope                 ExecutionScope      `json:"scope"`
	Origin                string              `json:"origin"`
	Tool                  string              `json:"tool"`
	ToolVersion           string              `json:"toolVersion"`
	SchemaHash            string              `json:"schemaHash"`
	Generation            string              `json:"generation"`
	ProviderCallID        string              `json:"providerCallId,omitempty"`
	SelectionRevision     uint64              `json:"selectionRevision,omitempty"`
	NodeExecutionID       string              `json:"nodeExecutionId,omitempty"`
	DefinitionRef         string              `json:"definitionRef,omitempty"`
	BindingRef            string              `json:"bindingRef,omitempty"`
	OperationID           string              `json:"operationId,omitempty"`
	EntryPoint            string              `json:"entryPoint,omitempty"`
	OriginalArgumentsHash string              `json:"originalArgumentsHash"`
	FinalArgumentsHash    string              `json:"finalArgumentsHash"`
	ArgumentsRef          string              `json:"argumentsRef"`
	FinalArguments        json.RawMessage     `json:"finalArguments,omitempty"`
	Resources             []ExecutionResource `json:"resources,omitempty"`
	Effect                string              `json:"effect"`
	Concurrency           string              `json:"concurrency"`
	BackendID             string              `json:"backendId"`
	Argv                  []string            `json:"argv,omitempty"`
	Cwd                   string              `json:"cwd,omitempty"`
	EnvironmentRef        string              `json:"environmentRef,omitempty"`
	StdinRef              string              `json:"stdinRef,omitempty"`
	Mounts                []ExecutionMount    `json:"mounts,omitempty"`
	TempRootRef           string              `json:"tempRootRef,omitempty"`
	OutputLimitBytes      int                 `json:"outputLimitBytes,omitempty"`
	Timeout               time.Duration       `json:"timeout,omitempty"`
	PolicyRef             string              `json:"policyRef"`
	RequestedGrantRef     string              `json:"requestedGrantRef,omitempty"`
}

type ExecutionResource struct {
	Identity        string `json:"identity"`
	ExpectedVersion string `json:"expectedVersion"`
}

type ExecutionMount struct {
	SourceRef string `json:"sourceRef"`
	Target    string `json:"target"`
	ReadOnly  bool   `json:"readOnly"`
}

func (f FrozenExecution) Clone() FrozenExecution {
	f.FinalArguments = append(json.RawMessage(nil), f.FinalArguments...)
	f.Resources = append([]ExecutionResource(nil), f.Resources...)
	f.Argv = append([]string(nil), f.Argv...)
	f.Mounts = append([]ExecutionMount(nil), f.Mounts...)
	return f
}

func (f FrozenExecution) Digest() (string, error) {
	f.Hash = ""
	raw, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
