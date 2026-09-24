package model

import "time"

// Limits holds the engineering protection defaults from the delivery plan.
type Limits struct {
	LogicalModelRequests   int
	TraceLogicalModelCalls int
	TraceTransportRequests int
	TraceToolCalls         int
	TraceCompactions       int
	OverflowRecoveries     int
	ActivityBudget         time.Duration
	ToolTimeout            time.Duration
	HookTimeout            time.Duration
	ReadConcurrency        int
	SubscriptionEvents     int
	SubscriptionBytes      int
	MaxCommitLineBytes     int
}

func DefaultLimits() Limits {
	return Limits{
		LogicalModelRequests:   3,
		TraceLogicalModelCalls: 128,
		TraceTransportRequests: 256,
		TraceToolCalls:         512,
		TraceCompactions:       8,
		OverflowRecoveries:     1,
		ActivityBudget:         30 * time.Minute,
		ToolTimeout:            120 * time.Second,
		HookTimeout:            10 * time.Second,
		ReadConcurrency:        4,
		SubscriptionEvents:     256,
		SubscriptionBytes:      2 << 20,
		MaxCommitLineBytes:     1 << 20,
	}
}

func (l Limits) WithDefaults() Limits {
	d := DefaultLimits()
	if l.LogicalModelRequests == 0 {
		l.LogicalModelRequests = d.LogicalModelRequests
	}
	if l.TraceLogicalModelCalls == 0 {
		l.TraceLogicalModelCalls = d.TraceLogicalModelCalls
	}
	if l.TraceTransportRequests == 0 {
		l.TraceTransportRequests = d.TraceTransportRequests
	}
	if l.TraceToolCalls == 0 {
		l.TraceToolCalls = d.TraceToolCalls
	}
	if l.TraceCompactions == 0 {
		l.TraceCompactions = d.TraceCompactions
	}
	if l.OverflowRecoveries == 0 {
		l.OverflowRecoveries = d.OverflowRecoveries
	}
	if l.ActivityBudget == 0 {
		l.ActivityBudget = d.ActivityBudget
	}
	if l.ToolTimeout == 0 {
		l.ToolTimeout = d.ToolTimeout
	}
	if l.HookTimeout == 0 {
		l.HookTimeout = d.HookTimeout
	}
	if l.ReadConcurrency == 0 {
		l.ReadConcurrency = d.ReadConcurrency
	}
	if l.SubscriptionEvents == 0 {
		l.SubscriptionEvents = d.SubscriptionEvents
	}
	if l.SubscriptionBytes == 0 {
		l.SubscriptionBytes = d.SubscriptionBytes
	}
	if l.MaxCommitLineBytes == 0 {
		l.MaxCommitLineBytes = d.MaxCommitLineBytes
	}
	return l
}
