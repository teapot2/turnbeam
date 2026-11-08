# Changelog

All notable changes to Turnbeam are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Turnbeam is experimental: until 1.0,
minor versions may change the CLI or the transcript JSON schema.

## [Unreleased]

## [0.1.0] - 2026-11-08

First release, for macOS on Apple Silicon.

### Added

- Transcribe a recorded meeting into a speaker-labeled transcript: Meta Muse
  transcribes the audio remotely, and NVIDIA's Nemotron 3 model identifies
  speakers on-device with Metal.
- Speaker matching calibrated against human annotations of the AMI meeting
  corpus; turns without a clearly dominant speaker are labeled Unknown instead
  of guessed.
- Output as Markdown, plain text, canonical JSON, or SRT and WebVTT subtitles,
  chosen with `--format` or from the output file's extension.
- Checkpoints after every transcribed chunk, so `--resume` continues an
  interrupted run without paying for completed chunks again.
- Offline `speakers rename` and `export` from the JSON sidecar.
- `setup` downloads the pinned runtime and model, verifies every file against a
  SHA-256 hash, and resumes interrupted downloads; `doctor` checks that
  everything is ready.
- `key set`, `key status` and `key remove` manage the Meta API key in the macOS
  Keychain; `META_API_KEY` takes precedence.
- Live progress, shell completion for zsh, bash and fish, and suggestions for
  mistyped commands.

[Unreleased]: https://github.com/teapot2/turnbeam/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/teapot2/turnbeam/releases/tag/v0.1.0
