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
guessed. Speakers are always identified on your Mac. Transcription runs on
your Mac too with a local model, or through Meta Muse by default.

> [!NOTE]
> Turnbeam is experimental. The CLI and the transcript JSON schema may change.

## Requirements

- A Mac with Apple Silicon.
- `ffmpeg` and `ffprobe` on your `PATH`.
- For the default Meta Muse model: a Meta API key, and permission to send the
  recording to Meta. Local models need neither.

## Install

Download the Apple Silicon build from the
[releases page](https://github.com/teapot2/turnbeam/releases) (`checksums.txt`
lists each archive's SHA-256), or with the GitHub CLI:

```sh
gh release download --repo teapot2/turnbeam --pattern 'turnbeam_*_darwin_arm64.tar.gz'
tar -xzf turnbeam_*_darwin_arm64.tar.gz
```

Move `turnbeam` into a folder on your `PATH`. The binary is not notarized, so
if a browser downloaded it, clear macOS's quarantine flag first:
`xattr -d com.apple.quarantine turnbeam`.

To build from source instead, you need Go 1.27+ and the Xcode Command Line
Tools (cgo is used for the Keychain):

```sh
go install github.com/teapot2/turnbeam/cmd/turnbeam@latest
```

`turnbeam completion --help` sets up tab completion for zsh, bash or fish.

## Quick start

Transcribe on your Mac, with no account:

```sh
turnbeam setup --model parakeet                             # speaker model (~114 MB) + Parakeet (714 MB)
turnbeam meeting.mp4 --output meeting.md --model parakeet
```

Or with Meta Muse, the default:

```sh
turnbeam setup                                              # speaker model (~114 MB)
turnbeam key set                                            # save your Meta API key in the Keychain
turnbeam meeting.mp4 --output meeting.md
```

`turnbeam doctor` checks that everything is installed, offline. If `setup` is
interrupted, run it again and it resumes where it stopped. Any file with an
audio track that FFmpeg can decode works as input. `TURNBEAM_MODEL=parakeet`
makes a local model the default, and a nonempty `META_API_KEY` environment
variable takes precedence over the Keychain.

## Output

```markdown
[00:00:00.200] **Speaker 1:** Thanks for joining. Let's start with the roadmap.

[00:00:04.900] **Speaker 2:** Sounds good. I have two updates.

[00:00:07.300] **Unknown speaker:** Sorry, can everyone hear me?
```

- `--format` picks `markdown`, `text`, `json`, or `srt`/`vtt` subtitles.
  Without it, the output file's extension decides, so `-o meeting.srt` writes
  subtitles.
- `--timestamps` shows each turn's `start` (the default), its `range`, or
  `none`.
- With `--output`, Turnbeam also writes a JSON sidecar
  (`meeting.transcript.json`) with exact timings and speaker IDs; renaming and
  re-exporting work from it. Without `--output`, the transcript goes to stdout
  and progress to stderr.
- Keep the default `--mode diarization`. `--mode endpointing` splits turns at
  pauses regardless of who is talking, so far fewer of them get a speaker.

`turnbeam --help` lists every option.

## Models

| Model | Runs | Languages | Word errors | Right speaker | Wrong speaker |
| --- | --- | --- | --- | --- | --- |
| `meta` (default) | Meta API | see Meta's documentation | 27% | 65% | 1.9% |
| `parakeet` | on your Mac, under 1 min for 21 min of audio | 25 European | 26% | 76% | 1.5% |
| `nemotron-3.5` | on your Mac, about 2 min | multilingual, incl. Chinese, Japanese, Hindi, Arabic | 30% | 75% | 1.5% |

Accuracy was measured on a 21-minute AMI meeting against human word-level
annotations: the share of words transcribed wrongly, and the share placed with
the right or with a wrong speaker.

`turnbeam models` lists the models and whether they are installed;
`setup --model NAME` adds one (714 MB for Parakeet, 742 MB for Nemotron 3.5),
and several can be installed side by side. Local models upload nothing, need no
key and cost nothing. They transcribe the whole recording in one pass, so
`--resume`, `--fresh`, `--mode` and `--concurrency` apply only to `meta`.
Nemotron 3.5 reports words about 0.4 s late, so Turnbeam moves its word times
300 ms earlier.

## How it works

1. **Prepare:** FFmpeg converts the recording to 24 kHz mono audio, keeping the
   original timeline.
2. **Find speakers:** NVIDIA's Nemotron 3 model runs on your Mac (Metal) over
   the whole recording, so speaker labels stay consistent from start to finish.
3. **Transcribe:** a local model transcribes the whole recording on your Mac.
   Meta Muse instead receives 8–9 minute chunks, cut only at pauses and sent in
   parallel. Completed chunks are checkpointed so an interrupted run can
   resume.
4. **Assemble:** words and turns go back on the original timeline and get a
   speaker only when the speaker timeline clearly supports it.

## Resume, rename and export

```sh
turnbeam meeting.mp4 --output meeting.md --resume
turnbeam speakers rename meeting.transcript.json --speaker 1=Alice --speaker 2=Bob
turnbeam export meeting.transcript.json --output meeting-named.md
```

`--resume` skips chunks a Meta run already transcribed, so they aren't billed
again; `--fresh` starts over and pays again. Renaming updates the JSON sidecar
in place; exporting never overwrites an existing file. Both run offline.

Exit codes: `0` success, `1` failure, `2` invalid usage, `130` canceled.

## Privacy

- Meta receives only the audio chunks, with your API key for authentication.
  Transcripts, speaker labels and file names stay on your Mac. With a local
  model, nothing leaves your Mac.
- Speaker labels are model guesses, not verified identities, and at most eight
  speakers are distinguished.
- Checkpoints of Meta runs in `~/Library/Caches/turnbeam/jobs` contain
  transcript text and are never deleted automatically. Local runs write none.

## Uninstall

```sh
turnbeam key remove
rm -rf ~/Library/Application\ Support/turnbeam ~/Library/Caches/turnbeam
```

Then delete the `turnbeam` binary (`$(go env GOPATH)/bin/turnbeam` if you used
`go install`). If you ran `setup --install-dir`, remove that directory instead
of the one in Application Support.

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
their notices are installed with them: NeMo-Speech.cpp is Apache-2.0, the
Nemotron 3 and Nemotron 3.5 models are OpenMDW-1.1, and the Parakeet TDT v3
model is CC-BY-4.0.
