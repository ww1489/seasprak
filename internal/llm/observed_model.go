package llm

// WithObservedTransportModel is a trusted assembly declaration that every
// physical request (including SDK retries and cache calls) uses
// NewObservedTransport. It does not certify capabilities or add a budget.
// Legacy injected models without this declaration retain call-level occupancy.
func WithObservedTransportModel(inner Model) Model { return &observedModel{Model: inner} }

type observedModel struct{ Model }

func (*observedModel) UsesObservedTransport() bool { return true }
func UsesObservedTransport(m Model) bool {
	observed, ok := m.(interface{ UsesObservedTransport() bool })
	return ok && observed.UsesObservedTransport()
}
