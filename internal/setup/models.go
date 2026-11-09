package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teapot2/turnbeam/internal/safeio"
)

// LocalModel is a pinned speech recognition model that runs on device. People
// choose it by Name: `setup --model NAME` installs it and `--model NAME`
// transcribes with it. Each model's files have their own names, so several
// can be installed side by side.
type LocalModel struct {
	Name      string // CLI name
	Title     string // display name
	File      string // upstream GGUF file name, recorded as transcript provenance
	Revision  string // pinned upstream revision
	License   string
	Languages string
	// Latency is how long after a word was spoken the model reports it. The
	// local engine subtracts it so each word is matched against the audio where
	// it was spoken. Measured against human word timings on the AMI meeting
	// ES2002a: Nemotron 3.5's times trailed by a median 410 ms (start) and 260 ms
	// (end), and correcting by 300 ms gave the most words the right speaker;
	// Parakeet's trailed by 80 ms, and no shift beat leaving them unchanged.
	// Nemotron EN's trailed by 360 ms and 220 ms, best corrected by 250 ms;
	// Parakeet CTC's by 80 ms and -40 ms, where no shift is needed.
	Latency time.Duration
	// assets lists the model file first, then its notices. Custom models have
	// none: file is their absolute path instead.
	assets []artifact
	file   string
}

// Size is the model file's download size in bytes, or 0 for a custom model.
func (m LocalModel) Size() int64 {
	if len(m.assets) == 0 {
		return 0
	}
	return m.assets[0].size
}

// Custom reports whether the model is a user-supplied GGUF file rather than a
// pinned download.
func (m LocalModel) Custom() bool { return m.file != "" }

// Path returns a custom model's file, or "" for a pinned model, which lives in
// the installation.
func (m LocalModel) Path() string { return m.file }

func (m LocalModel) path(dir string) string {
	if len(m.assets) == 0 {
		return m.file
	}
	return filepath.Join(dir, m.assets[0].name)
}

// present reports whether the model file exists in an installation, whether
// or not it and the model's notices still verify.
func (m LocalModel) present(root *os.Root) bool {
	if len(m.assets) == 0 {
		return false
	}
	_, err := root.Lstat(m.assets[0].name)
	return err == nil
}

// ErrCustomModel identifies a --model file that is not a readable GGUF file.
var ErrCustomModel = errors.New("custom model must be a readable, nonempty .gguf file")

// IsModelFile reports whether a --model value names a GGUF file rather than a
// model in the catalog.
func IsModelFile(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".gguf") }

// CustomModel describes a speech recognition GGUF file the user supplies, such
// as their own NeMo checkpoint converted for the pinned NeMo-Speech.cpp runtime.
// It is neither downloaded nor verified against a pin: its Revision records the
// file's SHA-256 so a transcript names exactly what produced it, and its word
// times are used uncorrected because its Latency is unknown.
func CustomModel(ctx context.Context, path string) (_ LocalModel, err error) {
	defer func() {
		if err != nil && ctx.Err() == nil {
			err = errors.Join(ErrCustomModel, err)
		}
	}()
	if !IsModelFile(path) {
		return LocalModel{}, fmt.Errorf("%s does not end in .gguf", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return LocalModel{}, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return LocalModel{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return LocalModel{}, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return LocalModel{}, errors.New("not a regular, nonempty file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, safeio.Reader(ctx, f)); err != nil {
		return LocalModel{}, err
	}
	if err := ctx.Err(); err != nil {
		return LocalModel{}, err
	}
	return LocalModel{Name: abs, Title: "Custom model", File: filepath.Base(abs), Revision: "sha256:" + hex.EncodeToString(hash.Sum(nil)), file: abs}, nil
}

// ErrLocalModel identifies a local model that is not installed or is damaged.
var ErrLocalModel = errors.New("local transcription model missing or damaged")

// ModelError names the local model that is missing or damaged, and the
// command that installs it.
type ModelError struct {
	Name string
	Err  error
}

func (e *ModelError) Error() string {
	return fmt.Sprintf("local model %s is not installed or is damaged; run `turnbeam setup --model %s`", e.Name, e.Name)
}

func (e *ModelError) Unwrap() []error { return []error{ErrLocalModel, e.Err} }

// ErrUnknownModel identifies a model name that is not in the pinned catalog.
var ErrUnknownModel = errors.New("unknown local model")

// LocalModels lists the pinned local models in display order.
func LocalModels() []LocalModel { return pinnedModels() }

// FindLocalModel returns the pinned local model with this CLI name.
func FindLocalModel(name string) (LocalModel, bool) {
	return pinnedCatalog().model(name)
}

func pinnedModels() []LocalModel {
	const parakeet = "541d1f99c6b0c3cd0b11a95167540bb8edefd82b"
	const nemotron = "1c8deaecc64b91f034d73e08dd8b64625eb3395d"
	const parakeetCTC = "20e63a0fed6aedba145b74b826dbd41df0941730"
	const nemotronEN = "ebe59e5a817142986528bbbee5dba8db7b38ed50"
	return []LocalModel{
		{
			Name: "parakeet", Title: "Parakeet TDT v3", File: "parakeet-tdt-0.6b-v3.q8_0.gguf", Revision: parakeet,
			License: "CC-BY-4.0", Languages: "25 European languages",
			assets: []artifact{
				{"asr-parakeet.gguf", "https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3/resolve/" + parakeet + "/parakeet-tdt-0.6b-v3.q8_0.gguf", "e3880d0aaaaf2c308ea2c35016b2b895c423eb3fda924c1b463d1c19b7f4d32e", 713975456},
				{"ASR-PARAKEET-CARD.md", "https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3/resolve/" + parakeet + "/README.md", "7a4306a43f395e3a716e77f1d81c03db18a0a355d7b566bb1d6307213ba0302b", 42888},
				{"ASR-PARAKEET-LICENSE", "https://raw.githubusercontent.com/spdx/license-list-data/31ba1a50e5397e00a304dbadc76531740e89ee48/text/CC-BY-4.0.txt", "d557539df68e771cc1eedcc91d13f70fca930e508d11eedcafa4b15db49e3744", 17023},
			},
		},
		{
			// Its OpenMDW-1.1 license is the MODEL-LICENSE already installed.
			Name: "nemotron-3.5", Title: "Nemotron 3.5 ASR", File: "nemotron-3.5-asr-streaming-0.6b.q8_0.gguf", Revision: nemotron,
			License: "OpenMDW-1.1", Languages: "multilingual, incl. zh, ja, hi, ar",
			Latency: 300 * time.Millisecond,
			assets: []artifact{
				{"asr-nemotron-3.5.gguf", "https://huggingface.co/nvidia/nemotron-3.5-asr-streaming-0.6b/resolve/" + nemotron + "/nemotron-3.5-asr-streaming-0.6b.q8_0.gguf", "a5c435f294eea8f88ce68dd27b8c3bfea7f777cb2fbba04fcd30eaa555f429ae", 741548352},
				{"ASR-NEMOTRON-3.5-CARD.md", "https://huggingface.co/nvidia/nemotron-3.5-asr-streaming-0.6b/resolve/" + nemotron + "/README.md", "a3344caadf796c084c6b90a9fa5978068fd45e3a019790bebe50489bb3c0f7b7", 54239},
			},
		},
		{
			Name: "parakeet-ctc", Title: "Parakeet CTC 1.1B", File: "parakeet-ctc-1.1b.q8_0.gguf", Revision: parakeetCTC,
			License: "CC-BY-4.0", Languages: "English",
			assets: []artifact{
				{"asr-parakeet-ctc.gguf", "https://huggingface.co/nvidia/parakeet-ctc-1.1b/resolve/" + parakeetCTC + "/parakeet-ctc-1.1b.q8_0.gguf", "6584fc0fdacf1c220401ea4c3a1d5b44454b655c141cb8672178072c203d92b8", 1178100960},
				{"ASR-PARAKEET-CTC-CARD.md", "https://huggingface.co/nvidia/parakeet-ctc-1.1b/resolve/" + parakeetCTC + "/README.md", "1c0d57c1d2d0f0ec0cda6775f7c091c317493208d83f25f2b77d41093cffb33a", 13026},
				{"ASR-PARAKEET-CTC-LICENSE", "https://raw.githubusercontent.com/spdx/license-list-data/31ba1a50e5397e00a304dbadc76531740e89ee48/text/CC-BY-4.0.txt", "d557539df68e771cc1eedcc91d13f70fca930e508d11eedcafa4b15db49e3744", 17023},
			},
		},
		{
			// The card states the governing NVIDIA Open Model License and links it.
			Name: "nemotron-en", Title: "Nemotron Speech Streaming EN", File: "nemotron-speech-streaming-en-0.6b.q8_0.gguf", Revision: nemotronEN,
			License: "NVIDIA Open Model License", Languages: "English",
			Latency: 250 * time.Millisecond,
			assets: []artifact{
				{"asr-nemotron-en.gguf", "https://huggingface.co/nvidia/nemotron-speech-streaming-en-0.6b/resolve/" + nemotronEN + "/nemotron-speech-streaming-en-0.6b.q8_0.gguf", "d9a01898d2a611c8764e23a1c2f45e70bbd5a425dc4de93692ac951dd603812d", 699872960},
				{"ASR-NEMOTRON-EN-CARD.md", "https://huggingface.co/nvidia/nemotron-speech-streaming-en-0.6b/resolve/" + nemotronEN + "/README.md", "7701f4b2f1c16542c8ceb2b3a61dd144032898c17f4dc9cc1bbecda6972edec8", 25987},
			},
		},
	}
}
