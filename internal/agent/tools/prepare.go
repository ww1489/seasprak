package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

func argumentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Ordinary tool arguments are JSON, not a schema document. The encoder sorts
// object keys and retains array order; UseNumber preserves integer precision.
func normalizeArguments(raw json.RawMessage) (json.RawMessage, any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return nil, nil, err
	}
	canonical, err := json.Marshal(value)
	return canonical, value, err
}

func (e *Executor) prepareArguments(ctx context.Context, def Definition, input string) (json.RawMessage, error) {
	raw := json.RawMessage(input)
	for _, prepare := range def.PrepareArguments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var err error
		raw, err = callPrepare(ctx, prepare, append(json.RawMessage(nil), raw...))
		if err != nil {
			return nil, err
		}
	}
	final, value, err := normalizeArguments(raw)
	if err != nil {
		return nil, product.NewError(product.CodeInvalidArgument, "tool arguments are invalid JSON")
	}
	if err := e.comp[def.Name].Validate(value); err != nil {
		return nil, product.NewError(product.CodeInvalidArgument, "tool arguments fail the final schema")
	}
	if def.Validate != nil {
		if err := callValidation(ctx, def.Validate, append(json.RawMessage(nil), final...)); err != nil {
			return nil, err
		}
	}
	return final, ctx.Err()
}

func callPrepare(ctx context.Context, prepare func(context.Context, json.RawMessage) (json.RawMessage, error), raw json.RawMessage) (out json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			out, err = nil, product.NewError(product.CodeInternal, "tool argument preparation failed")
		}
	}()
	out, err = prepare(ctx, raw)
	if err != nil {
		return nil, product.NewError(product.CodeInvalidArgument, "tool argument preparation failed")
	}
	return out, nil
}
func callValidation(ctx context.Context, validate func(context.Context, json.RawMessage) error, raw json.RawMessage) (err error) {
	defer func() {
		if recover() != nil {
			err = product.NewError(product.CodeInternal, "tool argument validation failed")
		}
	}()
	if err := validate(ctx, raw); err != nil {
		return product.NewError(product.CodeInvalidArgument, "tool argument validation failed")
	}
	return nil
}

func resolveDescription(ctx context.Context, def Definition, final json.RawMessage) (d ExecutionDescription, err error) {
	d = def.Execution.clone()
	if def.ResolveExecution != nil {
		defer func() {
			if recover() != nil {
				err = product.NewError(product.CodeInternal, "execution resolution failed")
			}
		}()
		d, err = def.ResolveExecution(ctx, append(json.RawMessage(nil), final...), d)
		if err != nil {
			return d, product.NewError(product.CodeResourceUnavailable, "execution resolution failed")
		}
	}
	d = d.clone()
	if d.Timeout < 0 || d.OutputLimitBytes < 0 {
		return d, product.NewError(product.CodeInvalidArgument, "execution description is invalid")
	}
	if d.BackendID == "" {
		d.BackendID = "trusted-run"
	}
	if d.Effect == "" {
		d.Effect = "unknown"
	}
	if d.Concurrency == "" {
		d.Concurrency = "exclusive"
		if d.Effect == "read" {
			d.Concurrency = "shared"
		}
	}
	return d, nil
}

func runBeforeHooks(ctx context.Context, hooks []func(context.Context, agent.FrozenExecution) error, frozen agent.FrozenExecution) (err error) {
	for _, hook := range hooks {
		if err := ctx.Err(); err != nil {
			return err
		}
		func() {
			defer func() {
				if recover() != nil {
					err = product.NewError(product.CodeInternal, "tool call hook failed")
				}
			}()
			if hookErr := hook(ctx, frozen.Clone()); hookErr != nil {
				err = product.NewError(product.CodePermissionDenied, "tool call hook denied execution")
			}
		}()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
