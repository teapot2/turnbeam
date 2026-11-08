<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/mark-dark.svg">
    <img src="assets/mark.svg" alt="Turnbeam logo" width="64" height="48">
  </picture>
</p>

<h1 align="center">Turnbeam</h1>

<p align="center">
  Speaker-labeled transcripts of recorded meetings, from the command line.
</p>

<p align="center">
  <a href="https://github.com/teapot2/turnbeam/actions/workflows/ci.yml"><img src="https://github.com/teapot2/turnbeam/actions/workflows/ci.yml/badge.svg" alt="CI status"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/teapot2/turnbeam" alt="Go version">
  <img src="https://img.shields.io/badge/platform-macOS%20%7C%20Apple%20Silicon-lightgrey" alt="Platform: macOS on Apple Silicon">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="License: MIT"></a>
</p>

Turnbeam turns a meeting recording into a transcript that says who spoke when.
Turns it can't attribute reliably are labeled **Unknown speaker** instead of
guessed.

> [!NOTE]
> Turnbeam is experimental. The CLI and the transcript JSON schema may change.

## How it works

1. **Prepare:** FFmpeg converts the recording to 24 kHz mono audio, keeping the
   original timeline.
2. **Find speakers:** NVIDIA's Nemotron 3 model runs locally (Metal) over the
   whole recording, so speaker labels stay consistent from start to finish.
3. **Split:** the audio is cut into 8–9 minute chunks, only at pauses.
4. **Transcribe:** Meta Muse transcribes the chunks remotely, in parallel.
   Completed chunks are checkpointed so an interrupted run can resume.
5. **Assemble:** turns go back on the original timeline and get a speaker only
   when the speaker timeline clearly supports it.

## Requirements

- A Mac with Apple Silicon.
- Go 1.27+ and the Xcode Command Line Tools (cgo is used for Keychain).
- `ffmpeg` and `ffprobe` on your `PATH`.
- A Meta API key, and permission to send the recording to Meta.

## Install

Download a prebuilt binary for Apple Silicon from the
[releases page](https://github.com/teapot2/turnbeam/releases) (`checksums.txt`
lists the SHA-256 of each archive), or with the GitHub CLI:

```sh
gh release download --repo teapot2/turnbeam --pattern 'turnbeam_*_darwin_arm64.tar.gz'
tar -xzf turnbeam_*_darwin_arm64.tar.gz
```

Then move `turnbeam` into a folder on your `PATH`. The binary is not notarized,
so if you downloaded it with a browser, clear macOS's quarantine flag first:
`xattr -d com.apple.quarantine turnbeam`.

Or build it yourself with Go:

```sh
go install github.com/teapot2/turnbeam/cmd/turnbeam@latest   # or, from a clone: go install ./cmd/turnbeam
```

For tab completion in zsh, bash or fish, see `turnbeam completion --help`.

## Quick start

```sh
turnbeam setup                              # download and verify the model (~114 MB)
turnbeam doctor                             # check dependencies, offline
turnbeam key set                            # save your Meta API key in the Keychain
turnbeam meeting.mp4 --output meeting.md    # transcribe
```

If `setup` is interrupted or loses its connection, run it again: it resumes
where it stopped. A nonempty `META_API_KEY` environment variable takes
precedence over Keychain.
Any file with an audio track that FFmpeg can decode works as input.

## Example output

```markdown
[00:00:00.200] **Speaker 1:** Thanks for joining. Let's start with the roadmap.

[00:00:04.900] **Speaker 2:** Sounds good. I have two updates.

[00:00:07.300] **Unknown speaker:** Sorry, can everyone hear me?
```

With `--output`, Turnbeam also writes a JSON sidecar (`meeting.transcript.json`)
with exact timings and speaker IDs; renaming and re-exporting work from it.
Without `--output`, only the transcript goes to stdout and progress goes to
stderr. Use `--format` (`markdown`, `text`, `json`, or `srt`/`vtt` subtitles)
and `--timestamps` (`start`, `range`, `none`) to change the output; without
`--format`, the output file's extension picks it, so `-o meeting.srt` writes SRT
subtitles. `turnbeam --help` lists all options. Keep the default `--mode
diarization`: `--mode endpointing` splits turns at pauses regardless of who is
talking, so far fewer of them get a speaker.

## Resume, rename and export

```sh
turnbeam meeting.mp4 --output meeting.md --resume
turnbeam speakers rename meeting.transcript.json --speaker 1=Alice --speaker 2=Bob
turnbeam export meeting.transcript.json --output meeting-named.md
```

`--resume` skips chunks that were already transcribed, so they aren't billed
again; `--fresh` starts over and pays again. Renaming updates the JSON sidecar
in place; exporting never overwrites an existing file. Both run offline.

Exit codes: `0` success, `1` failure, `2` invalid usage, `130` canceled.

## Privacy

- Meta receives only the audio chunks, with your API key for authentication.
  Transcripts, speaker labels and file names stay on your Mac.
- Speaker labels are model guesses, not verified identities, and at most eight
  speakers are distinguished.
- Checkpoints in `~/Library/Caches/turnbeam/jobs` contain transcript text and
  are never deleted automatically.

## Uninstall

```sh
turnbeam key remove
rm -rf ~/Library/Application\ Support/turnbeam ~/Library/Caches/turnbeam
```

Then delete the `turnbeam` binary (`$(go env GOPATH)/bin/turnbeam` if you used
`go install`).

## Development

```sh
make check       # formatting, vet, race tests and build, as CI runs them
make fmt         # fix formatting and imports
make vulncheck   # scan for known vulnerabilities (needs network)
make build       # build ./bin/turnbeam
```

Default tests are offline and need no recordings, model or API key. Optional
native tests and coding conventions are in [AGENTS.md](AGENTS.md).

## Security

Please report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). Components that `setup` downloads keep their own licenses, and
their notices are installed with them: NeMo-Speech.cpp is Apache-2.0 and the
Nemotron 3 model is OpenMDW-1.1.
