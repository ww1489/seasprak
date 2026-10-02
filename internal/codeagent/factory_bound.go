package codeagent

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
)

// The finite per-call seam is the real final backend open. Public factories
// always supply jsonl.OpenBound; there is no path retry or alternate factory.
type codeBoundOpener func(string, *storage.ResourceRoots, *os.File, storage.Header, jsonl.Options) (*jsonl.Store, error)

func openCreateFileStore(ctx context.Context, opts Options, header storage.Header, openChild func(*os.Root, string) (*os.Root, error), openBound codeBoundOpener) (out *jsonl.Store, err error) {
	roots, err := storage.OpenResourceRoots(opts.StateRoot, storage.ResourceCode, opts.SessionID, true, openChild)
	if err != nil {
		return nil, factoryError(err)
	}
	defer func() {
		if roots != nil {
			err = closeFactoryOwners(err, roots)
		}
	}()
	if _, err := roots.Resource.Lstat("journal.jsonl"); err == nil {
		return nil, product.NewError(product.CodeStateConflict, "session already exists")
	} else if !os.IsNotExist(err) {
		return nil, factoryError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backend, err := openBound(opts.SessionID, roots, nil, header, jsonl.Options{MaxLine: opts.Limits.WithDefaults().MaxCommitLineBytes})
	if err != nil {
		return nil, factoryError(err)
	}
	roots = nil
	return backend, nil
}

func validateCodePrefix(inspected *os.File, id string, maxLine int, binding workspaceBinding) error {
	info, err := inspected.Stat()
	if err != nil {
		return err
	}
	stored, err := jsonl.ReadFile(io.NewSectionReader(inspected, 0, info.Size()), id, maxLine)
	if err != nil {
		return err
	}
	prefixBinding, err := decodeBinding(stored.Header.Workspace)
	if err != nil {
		return err
	}
	if prefixBinding != binding {
		return product.NewError(product.CodeIncompatibleVersion, "workspace binding changed while inspecting")
	}
	for _, commit := range stored.Commits {
		if err := state.ValidateCodeCommitCompatibility(commit); err != nil {
			return err
		}
	}
	return nil
}

func checkHeaderLeaf(root *os.Root) (os.FileInfo, error) {
	info, err := root.Lstat("journal.jsonl")
	if os.IsNotExist(err) {
		return nil, product.NewError(product.CodeNotFound, "session does not exist")
	}
	if err != nil {
		return nil, product.NewError(product.CodeStorageUnavailable, "session header is unavailable")
	}
	if !info.Mode().IsRegular() || storage.IsReparseInfo(info) {
		return nil, product.NewError(product.CodeIncompatibleVersion, "session header has invalid file type")
	}
	return info, nil
}

func headerRootError(err error) error {
	if os.IsNotExist(err) {
		return product.NewError(product.CodeNotFound, "session does not exist")
	}
	var pe *product.Error
	if errors.As(err, &pe) && pe.Code == product.CodeInvalidArgument {
		return product.NewError(product.CodeIncompatibleVersion, "session header path is invalid")
	}
	return product.NewError(product.CodeStorageUnavailable, "session header is unavailable")
}

func headerReaderError(err error) error {
	var pe *product.Error
	if errors.As(err, &pe) {
		if pe.Code == product.CodeInvalidArgument {
			return product.NewError(product.CodeIncompatibleVersion, "session header changed while opening")
		}
		if pe.Code == product.CodeNotFound {
			return product.NewError(product.CodeNotFound, "session does not exist")
		}
	}
	return factoryError(err)
}

// Attempt every owned Close even after a failure. Preserve the principal
// error, and never expose raw OS paths or a joined public product error.
func closeFactoryOwners(err error, owners ...io.Closer) error {
	for _, owner := range owners {
		if closeErr := owner.Close(); err == nil {
			err = closeErr
		}
	}
	return factoryError(err)
}

func factoryError(err error) error {
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
	return product.NewError(product.CodeStorageUnavailable, "session factory storage is unavailable")
}
