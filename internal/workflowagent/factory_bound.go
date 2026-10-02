package workflowagent

import (
	"context"
	"errors"
	"os"
	"reflect"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
)

// All factory-owned resources are attempted even when an earlier close fails.
// Injected stores never enter this cleanup path on construction failure.
func closeWorkflowFactoryOwned(cause error, backend storage.Store, inspected *os.File, roots *storage.ResourceRoots) error {
	primary := cause
	if inspected != nil {
		cause = errors.Join(cause, inspected.Close())
	}
	if backend != nil {
		cause = errors.Join(cause, backend.Close())
	}
	if roots != nil {
		cause = errors.Join(cause, roots.Close())
	}
	if primary != nil {
		return normalizeWorkflowFactoryError(primary)
	}
	return normalizeWorkflowFactoryError(cause)
}

func normalizeWorkflowFactoryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var pe *product.Error
	if errors.As(err, &pe) {
		return pe
	}
	return product.NewError(product.CodeStorageUnavailable, "workflow store operation failed")
}

// A retained file may receive legitimate commits between inspection and Load.
// Its initial facts and header must still be exactly those already authorized.
func replayWorkflowFactoryLatest(latest, inspected storage.StoredSession, checked runState, id string) (runState, error) {
	if err := storage.ValidateHeader(latest.Header, storage.ResourceWorkflow, id); err != nil {
		return runState{}, err
	}
	after, err := replay(latest, id)
	if err != nil {
		return runState{}, err
	}
	if after.Initial.Principal != checked.Initial.Principal {
		return runState{}, product.NewError(product.CodePermissionDenied, "principal does not own this workflow run")
	}
	if !reflect.DeepEqual(latest.Header, inspected.Header) || !reflect.DeepEqual(after.Initial, checked.Initial) {
		return runState{}, product.NewError(product.CodeIncompatibleVersion, "workflow initialization changed after inspection")
	}
	return after, nil
}
