# AGENTS.md

Follow the global Karpathy rules for thinking before coding, minimal implementation, focused changes, and verification. This file adds project-specific requirements only.

## Project and Required Reading

- This project is a Go Agent SDK bound to an explicit workspace. The module is `github.com/ww1489/seasprak`, and the toolchain is Go 1.27.0.
- Requirements are in `docs/pi-eino-prd/`; design, defaults, and acceptance criteria are in `docs/pi-eino-dev-plan/`. Judge delivered capabilities by current source code and this project's tests, not by documentation coverage or upstream tests as substitutes for product certification.
- Before creating or refining a plan, starting or resuming development, or delegating implementation, you must read and follow the [Operational Development Planning and Execution Guidelines](docs/development-plan-guidelines.md). These are execution rules, not merely reference material.

## Architecture and Dependencies

- Dependencies flow from `session → agent → model`. `model` must not import `agent`, `session`, or `internal/storage`; `agent` must not import `session` or `internal/storage`; `session/history` must not import `agent/eino` or `internal/storage`. Tests in `internal/architecture` enforce these boundaries.
- Storage implementations depend on interfaces in `session/history`; the history layer must not depend on concrete storage implementations. Define execution ports in `agent` and Eino adapters in `agent/eino`. Ports use execution-layer types, not `AgentSession` or history-manager pointers.
- Reuse Eino's Agentic execution path rather than writing another ReAct loop. Identify framework gaps with failing tests and fix the adapter without weakening permission, budget, or cancellation rules.
- Pin dependency versions in `go.mod` / `go.sum`. Release builds must not contain `replace` directives pointing to local absolute paths.

## Sessions and Security

- Require an explicit workspace; do not default to the process working directory. Opening a session preserves its original binding. Read-only browsing must not write, change the binding, or automatically resume execution.
- Public errors use existing codes from `model`. Error details, source code, fixtures, documentation, logs, and test output must not contain secrets, tokens, or values from `.test_env`.
- `.test_env` must be ignored by Git and used only for local `go test -tags live ./model/live` runs. It must not enter commits or CI.

## Testing and Cross-Platform Compatibility

- Start behavior changes with a test that reproduces the defect. Assert state, error codes, and actual invocation counts, not just output text. Tests must actually invoke newly added implementation functions. Place tests in the corresponding package's `*_test.go` files and include them in the default suite.
- Default tests use `internal/testkit`, temporary directories, and controllable test doubles without network access.
- Linux, Windows, and macOS compatibility is mandatory. Prefer the standard library and account for differences in paths, case sensitivity, line endings, permissions, file locks, and process behavior. Check availability before using a shell or external command.
- Isolate platform implementations with build constraints and `*_windows.go` / `*_unix.go`, distinguishing Linux from macOS when necessary. All three platforms require build and test evidence from CI or actual environments, with runtime tests for platform-specific behavior. Cross-compilation does not prove runtime compatibility.

## Required Verification

Before completing implementation, committing, or opening a pull request, run these checks from the repository root:

- Formatting: `gofmt -l .` must produce no output; otherwise run `go fmt ./...` and check again.
- Static analysis and build: `go vet ./...` and `go build ./...`.
- Full test suites: `go test ./...` and `go test -race ./...`. For concurrency changes, first run race tests for the affected packages, then the full suite. There is no separate smoke-test entry point.
- Security: `govulncheck ./...` (`golang.org/x/vuln/cmd/govulncheck`).
- Diff hygiene: `git diff HEAD --check`. This does not cover untracked files; also check new files for secrets, conflict markers, and content that must not be committed.
- Live models: if `.test_env` exists at the repository root, you must run `go test -tags live ./model/live` and satisfy its assertions before completing implementation. If the file is absent, report the skip. Do not create credentials or echo their values.

All checks above are mandatory. Address and report failures honestly; do not narrow the test scope and then claim the entire repository passes. If tools, external services, or platform runtime environments are unavailable, report the command, exit code (or that it was not run), relevant output, and unverified items, then ask the maintainer how to proceed. Skipped checks are not passes. List verification commands and results in the completion response.

CI does not require golangci-lint. If you run it, address only findings relevant to the current task and report unrelated suggestions separately.

## Documentation and Commits

- Change `docs/` only as required by the task, distinguishing implemented capabilities from those not yet delivered. Use the authoritative design definitions for identities, error codes, and budget values rather than creating conflicting specifications.
- Commit or open a pull request only when explicitly requested by the user. Explain the reason for the changes in complete sentences in the commit message. Never commit `.test_env`, `*.exe`, `*.test`, or `bin/`.
