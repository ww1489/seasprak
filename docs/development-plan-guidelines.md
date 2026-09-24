# Operational Development Planning and Execution Guidelines

Reading this document is required by [AGENTS.md](../AGENTS.md). It supplements the global Karpathy principles with requirements for planning detail, delegation, and completion criteria. AGENTS.md remains authoritative for project verification commands and security requirements.

## 1. Core Standard

**Every small step must explain exactly how to do the work, not merely what to achieve.** The implementer should be able to locate the code, act in order, handle failures, and verify the result without guessing critical requirements. "Analyze → implement → test" and "implement serial scheduling" are headings, not executable steps.

Split tasks by independently verifiable behavior. Further divide multiple independent actions into numbered substeps. If the user requires the original plan to remain unchanged, add detail in separate execution notes without editing that plan or duplicating existing to-dos.

## 2. Verify Before Planning Implementation

- Check requirements, design, and current call paths to establish modification points, state ownership, commit points, existing tests, and task boundaries. Clearly label planned new files or interfaces rather than describing them as existing facts.
- Prefer the standard library, existing dependencies, and project implementations. Verify actual versions and cancellation, waiting, retry, and callback semantics; do not infer guarantees from interface names. Explain why existing implementations are insufficient before building a new capability.
- Turn unresolved critical decisions into investigation steps: where to look, which questions to answer, how to verify findings, and what criteria guide the choice. Confirm the conclusion before implementing dependent steps.

## 3. Format for Each Small Step

Use the title "Step N: Specific Action" and the fields below. Explain any field that genuinely does not apply:

1. **Prerequisites**: Required steps, established facts, and checks that have already passed.
2. **Targets and inputs**: Exact files, functions, interfaces, and input sources.
3. **Action sequence**: What to change and which component owns the responsibility, how data flows, and the order of calls and state transitions. Split multiple actions further.
4. **Constraints to preserve**: Areas that must not change, plus interface, permission, state-ownership, concurrency, and platform boundaries.
5. **Failure handling**: How to handle errors, cancellation, timeouts, duplicate requests, and races; when side effects are allowed and which facts must be retained.
6. **Expected deliverables**: Inspectable behavior, tests, interface contracts, or investigation conclusions.
7. **Acceptance criteria**: Test locations, normal/edge/failure scenarios, key assertions, commands, expected results, and runtime platforms. These must pass before proceeding to dependent steps.

Writing every line of code in advance is unnecessary, but state ownership, commit order, permission decisions, and failure semantics must not be left for the implementer to decide on the fly.

## 4. Example of Step Granularity: Cancelling Execution

This example illustrates decomposition only. For an actual task, verify the current implementation and fill in the fields above. It is not a new to-do list.

1. Read `session/session.go` and `session/execution.go` to locate the execution context, cancel function, exit notification, and mailbox entry point. Establish who modifies state and avoid creating a second copy of runtime state.
2. Block the model with a channel and queue a second task. Distinguish "cancellation received" from "execution exited." First make the test fail because of the target defect; assert that no cancelled terminal state appears before exit and that the second task does not execute.
3. Validate task state and commit cancellation intent in the mailbox handler for `Cancel`. Inject a commit failure and assert that visible state does not change prematurely.
4. Invoke the active execution's cancel function and wait for exit outside the mailbox, allowing the mailbox to continue receiving execution facts. Verify that there is no deadlock or premature exit confirmation, and that a caller's wait timeout does not imply execution has stopped.
5. After receiving the exit result, have the coordinator commit the terminal state, undelivered inputs, scheduling holds for the existing queue, and the unique terminal event together, then publish events. Verify that repeated cancellation does not repeat finalization and that terminal tasks cannot resume running.
6. Complete normal, failure, and race assertions. Run `go test ./session -count=1` and `go test -race ./session -count=1`, followed by the full verification required by AGENTS.md.

## 5. Delegation and Completion

- Delegate numbered steps at the granularity above, including necessary context, verified facts, allowed and prohibited changes, shared interfaces, and task dependencies. The implementer must not depend on implicit context from the parent conversation. Establish shared contracts before implementing dependent work in parallel.
- Require a report of each step's status, code and interface changes, verification commands and results, unresolved issues, and blockers. Report and obtain confirmation before exceeding scope or changing a critical contract.
- Check the actual call path and perform integration verification after completing each behavior. Defining a function or hook does not prove it is wired into execution. The primary implementer must verify the delivered work rather than treating a delegated task's report as completion evidence.
- Verify acceptance against the original requirements and plan, not just newly added tests: tests and implementation may share a faulty assumption. Mark work complete only after deliverables, assertions, and required checks all pass. Unverified, failing, or blocked work remains incomplete, with the reason recorded.

Before delivering a plan, check whether the implementer knows how to perform every step, what must not change, how to handle failures, and how to prove completion. Fill any gaps before executing or delegating.
