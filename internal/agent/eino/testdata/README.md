# Legacy scalar checkpoint and regression evidence

`checkpoint_scalar_v0.9.21.b64` contains a real Eino v0.9.21 TurnLoop checkpoint after a graceful stop after the model and before tools. All data is synthetic. The four Extra/Extension locations contain `synthetic-legacy-scalar`.

## Provenance

Captured on Windows/amd64 with Go 1.27.0, 2026-09-29, from repository HEAD `a8fa57fef3d544482e2e4998d5671b17058e645e` using the existing `checkpoint_regression_test.go` harness and a test-only scalar decorator. The production `checkpoint.go` was restored through a Go build overlay to its exact pre-fix HEAD contents. Both `git hash-object` of the overlay source and `git rev-parse HEAD:internal/agent/eino/checkpoint.go` returned `6abe582b065913b3bbbeaccc029e00c83665bdd0`. Thus the four new production type registrations did not run during capture. Dependencies remained pinned to the working tree's Eino v0.9.21; no dependency upgrade or serializer replacement was used.

Capture command (exit 0):

`go test -overlay=C:/Users/admin/AppData/Local/Temp/seasprak-checkpoint-overlay.json ./internal/agent/eino -run '^TestCaptureLegacyScalarCheckpoint$' -count=1`

The temporary capture test used `newCheckpointHarness(true)`, decorated the assistant with the scalar at all four locations, pushed `checkpointPrompt`, waited for model entry, called `loop.Stop(adk.WithGraceful())`, asserted a successful interrupted checkpoint, and wrote the actual store bytes as base64 using `O_EXCL` to prevent overwrites. The capture-only test was removed. The overlay maps only `internal/agent/eino/checkpoint.go` to the exact old source; it is not used by normal tests.

`TestCheckpointLegacyScalarResume` reads the immutable fixture without generating a checkpoint, validates its paused envelope, invokes real GenResume, asserts the four scalar values, one remaining tool execution, one follow-up model call seeing the tool result, and zero GenInput calls. This certifies the old after-model scalar checkpoint, not all historic formats or a complete old disk session.

## Before-fix evidence and supported boundary

Before adding production registrations, Windows normal and race runs of `go test [-race] ./internal/agent/eino -run '^TestCheckpointExtraResume$' -count=1` both failed (exit 1). Both pause points failed for nested maps with `gob: type not registered for interface: map[string]interface {}`; the type matrix failed in Eino's state copy with `unknown type: json.Number`.

After static registration, direct gob round-trips preserve the entire strict type matrix, including `json.Number("9007199254740993")`, `json.RawMessage`, bytes, and nested JSON trees. Real resume preserves the nested trees and json.Number's exact concrete type and text. Eino v0.9.21 `compose.deepCopyState` uses `internal/serialization.InternalSerializer`, whose slice branch records only the element type and reconstructs with `reflect.SliceOf`; it converts named RawMessage slices to plain []byte before gob persistence. The initial strict real-resume RawMessage assertions failed at all four locations (exit 1). The bounded acceptance explicitly asserts []byte with identical content for this one field; it does not claim end-to-end RawMessage support. This internal framework behavior is separate from the product's existing JSON serialization semantics, which are unchanged.
