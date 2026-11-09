# Security Policy

## Supported versions

Only the latest release is supported. Security fixes land on the `main`
branch and ship in the next release; please test against the latest release or
the latest commit on `main` before reporting.

## Reporting a vulnerability

Please do not open a public issue for security problems. Report them privately
through GitHub: open the repository's **Security** tab and choose
**Report a vulnerability**.

Include the version or commit you tested, the steps to reproduce, and the
impact you observed. Never include a real API key or a private recording; use a
revoked key and generated or public audio instead.

Turnbeam is a small experimental project, so reports are handled on a
best-effort basis. You will be credited in the fix unless you ask not to be.

## Scope

In scope:

- The Meta API key leaking into command-line arguments, logs, error messages,
  child processes, checkpoints or output files.
- `setup` accepting downloads that do not match the pinned SHA-256 hashes, or
  writing outside the installation directory while extracting.
- Checkpoints, transcripts or staging files that other local users can read, or
  that are written outside the paths you chose.
- Transcript output that can inject terminal control sequences.

Out of scope:

- How Meta stores or processes uploaded audio. See Meta's own terms.
- Bugs in NeMo-Speech.cpp or the Nemotron and Parakeet models; please report
  them upstream.
- Attacks that already require control of your macOS user account.

## Security design

- The API key comes from `META_API_KEY` or a macOS Keychain item that does not
  sync to iCloud. It is never passed as an argument, and `META_*` variables are
  removed from every child process's environment.
- Requests go only to Meta's HTTPS endpoint, with redirects and cookies disabled.
  Error messages never include response bodies, URLs, local paths or the key.
- Every downloaded runtime and model file is checked against a size and SHA-256
  hash pinned in the source, and archive extraction is confined to the
  installation directory.
- Checkpoint and installation directories must be private (mode `0700`), and
  checkpoint and staged output files are created with mode `0600`.
