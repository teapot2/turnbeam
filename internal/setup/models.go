package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
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
	Latency time.Duration
	// assets lists the model file first, then its notices.
	assets []artifact
}

// Size is the model file's download size in bytes.
func (m LocalModel) Size() int64 { return m.assets[0].size }

func (m LocalModel) path(dir string) string { return filepath.Join(dir, m.assets[0].name) }

// present reports whether the model file exists in an installation, whether
// or not it and the model's notices still verify.
func (m LocalModel) present(root *os.Root) bool {
	_, err := root.Lstat(m.assets[0].name)
	return err == nil
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
	}
}
