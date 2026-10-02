package codeagent

import "github.com/ww1489/seasprak/internal/codeagent/state"

// These aliases expose existing snapshot and operation values to controlled
// consumers without exposing the state manager or concrete storage backends.
type (
	OperationReceipt = state.OperationReceipt
	Invocation       = state.Invocation
)
