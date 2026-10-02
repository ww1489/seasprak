package eino

import (
	"bytes"
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	product "github.com/ww1489/seasprak/internal/errors"
)

// validateNativeCheckpoint lets the pinned framework load its opaque state into
// an inert agent. Name stops the synchronous load before buildResumeInfo,
// lifecycle callbacks or execution; the consumed context is never reused.
func validateNativeCheckpoint(data []byte, targetName string) error {
	probe := &checkpointValidationAgent{name: targetName}
	return probe.validate(checkpointValidationStore{data: bytes.Clone(data)})
}

func (a *checkpointValidationAgent) validate(store adk.CheckPointStore) (err error) {
	err = checkpointValidationInvalid()
	if a.name == "" {
		return err
	}
	// A non-zero-size object is private to this load. Equal marker values do
	// not authorize another invocation's panic signal.
	a.stop = &checkpointValidationStop{marker: 1}
	defer func() {
		err = checkpointValidationResult(recover(), a.stop, a.checked, a.valid)
	}()
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: a, CheckPointStore: store})
	// Any load error or normal fall-through is rejection, including a future
	// framework that absorbs Name's signal. Never try a second load or resume.
	_, _ = runner.ResumeWithParams(context.Background(), "validate", &adk.ResumeParams{Targets: map[string]any{}})
	return err
}

func checkpointValidationInvalid() error {
	return product.NewError(product.CodeIncompatibleResume, "checkpoint has no compatible native agent state")
}

// Keep the actual recover guard explicit: comparing arbitrary panic values may
// itself panic, and checked/valid alone cannot authenticate the stopping signal.
func checkpointValidationResult(signal any, stop *checkpointValidationStop, checked, valid bool) error {
	if caught, ok := signal.(*checkpointValidationStop); ok && caught == stop && checked && valid {
		return nil
	}
	return checkpointValidationInvalid() // Never expose panic values, bytes or stacks.
}

type checkpointValidationStop struct{ marker byte }

type checkpointValidationStore struct{ data []byte }

func (s checkpointValidationStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	if key != "validate" {
		return nil, false, nil
	}
	return bytes.Clone(s.data), true, nil
}
func (checkpointValidationStore) Set(context.Context, string, []byte) error {
	return product.NewError(product.CodeIncompatibleResume, "checkpoint validation cannot save execution state")
}

type checkpointValidationAgent struct {
	name           string
	stop           *checkpointValidationStop
	checked, valid bool // Written synchronously during this private native load.
}

func (a *checkpointValidationAgent) Name(ctx context.Context) string {
	ctx = adk.AppendAddressSegment(ctx, adk.AddressSegmentAgent, a.name)
	wasInterrupted, hasState, state := compose.GetInterruptState[[]byte](ctx)
	a.checked = true
	a.valid = wasInterrupted && hasState && len(state) != 0
	// AppendAddressSegment consumes this load's interruption map. Always stop
	// here; continuing into buildResumeInfo would reuse that consumed state.
	panic(a.stop)
}
func (*checkpointValidationAgent) Description(context.Context) string {
	return "Validate native checkpoint without execution"
}
func (*checkpointValidationAgent) Run(context.Context, *adk.TypedAgentInput[*schema.AgenticMessage], ...adk.AgentRunOption) *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]] {
	return checkpointValidationIterator()
}
func (*checkpointValidationAgent) Resume(context.Context, *adk.ResumeInfo, ...adk.AgentRunOption) *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]] {
	return checkpointValidationIterator()
}

func checkpointValidationIterator() *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]] {
	iter, gen := adk.NewAsyncIteratorPair[*adk.TypedAgentEvent[*schema.AgenticMessage]]()
	gen.Send(&adk.TypedAgentEvent[*schema.AgenticMessage]{Err: checkpointValidationInvalid()})
	gen.Close()
	return iter
}
