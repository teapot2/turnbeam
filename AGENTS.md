# Turnbeam

Go/Cobra CLI for prerecorded meetings on macOS ARM64: FFmpeg/FFprobe prepare media, Meta Muse transcribes, and NeMo-Speech.cpp/Nemotron 3 diarizes the recording.

## Context and Scope
- Read available specifications and relevant code before pipeline changes; disclose missing requirements. Read `.codex/private-context.md` if present, but do not require it.
- Build narrow vertical slices. Keep documentation minimal: no mirrored specs or per-ticket READMEs. Versioned text must contain no private workspace links, internal ticket references, or private recording metadata.
- Keep disposable tools and evidence under ignored `.cache/spikes/`; replace them with product tests, then delete the spike. `testdata/` also stays local and ignored; fixture check-in is not a completion gate.

## Engineering
- Prefer the standard library; justify dependencies, version `go.mod`/`go.sum`, and avoid unrelated upgrades. Keep CLI handlers thin, logic in focused internal packages, and interfaces at consuming boundaries only when needed.
- Use idiomatic Go, `gofmt`, and concise comments for exported declarations or non-obvious behavior. Return contextual errors preserving `errors.Is`/`errors.As`; no panic, `log.Fatal`, or `os.Exit` in processing packages.
- Pass `context.Context` first; bound concurrency, propagate cancellation, and terminate goroutines and child processes.
- Stdout is requested output only; progress/errors go to stderr. Preserve original timestamps and deterministic chronology. Meta speaker labels are request-local, never global identities; ambiguous attribution stays unknown, with no invented timings or confidence.
- Preserve versioned canonical JSON and offline rename/export. Resume only compatible checkpoints. Write artifacts atomically; never report incomplete multi-file output as complete.

## Verification
- Formatting/imports: `make fmt` fixes non-ignored Go sources; `make fmt-check` checks without rewriting files and runs in CI. The `goimports` development tool is pinned in `go.mod`/`go.sum`.
- For Go changes: `gofmt`, then `go test ./...`, `go vet ./...`, and `go build ./...` from the module root. Run `go test -race ./...` for concurrency changes and before release.
- Use focused standard-library tests (`t.TempDir`, `httptest`, subprocess fakes). Default tests must be offline, deterministic, and independent of local data.
- Optional native media checks: `TURNBEAM_TEST_NATIVE_MEDIA=1 go test ./internal/media -run TestNativeMedia` (FFmpeg/FFprobe required; generated local media only).
- Optional local pipeline checks: `TURNBEAM_TEST_NATIVE_PIPELINE=1 go test ./internal/pipeline -run TestNativePipeline -v` (installed FFmpeg and managed Metal runtime/model required; generated MP4 by default, optional `TURNBEAM_NATIVE_AUDIO`/`TURNBEAM_NATIVE_INSTALL_DIR`; transcription is always mocked locally).
- Optional archived API checks: `TURNBEAM_TEST_LOCAL_FIXTURES=1 go test ./internal/meta -run TestLocalCaptures` (local, ignored `testdata/meta/` required; no API calls).
- Cover affected malformed responses, retries, cancellation, timestamp boundaries, ambiguous speakers, checkpoint mismatches, and partial writes.
- Review the diff; preserve unrelated work. Report checks, missing prerequisites, failures, and remaining live-service/native-hardware validation. Distinguish proposed, mock-tested, and real behavior.
- Update these instructions only when durable commands or conventions change; keep architecture in the specification.
