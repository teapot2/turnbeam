// Package jobs stores private, explicit recovery generations, never temporary audio.
package jobs

import (
	"errors"
	"strings"
	"time"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/setup"
)

const schemaVersion = 1

var (
	ErrOptions  = errors.New("invalid recovery options")
	ErrMissing  = errors.New("no recovery checkpoint; start a normal or fresh run")
	ErrMismatch = errors.New("recovery input or inference settings changed; use --fresh")
	ErrCorrupt  = errors.New("damaged recovery checkpoint; use --fresh")
	ErrBusy     = errors.New("another job is using this recovery slot")
	ErrState    = errors.New("invalid or closed recovery state")
)

// Settings contains only inference-affecting provenance, not credentials, paths,
// display options or concurrency. Changes require an explicitly fresh generation.
type Settings struct {
	MetaModel        string `json:"metaModel"`
	MetaMode         string `json:"metaMode"`
	DiarizationModel string `json:"diarizationModel"`
	ModelRevision    string `json:"modelRevision"`
	RuntimeVersion   string `json:"runtimeVersion"`
	Preset           string `json:"preset"`
	MediaVersion     string `json:"mediaVersion"`
	ParserVersion    string `json:"parserVersion"`
	ChunkingVersion  string `json:"chunkingVersion"`
}

// DefaultSettings records the current pinned models and processing algorithms.
func DefaultSettings(mode string) Settings {
	if mode == "" {
		mode = meta.Diarization
	}
	return Settings{meta.Model, mode, "Nemotron-3-Diarization.q8_0.gguf", setup.ModelRevision, setup.RuntimeVersion, diarization.Preset, media.Version, diarization.ParserVersion, chunking.Version}
}

func (s Settings) valid() bool {
	if s.MetaMode != meta.Diarization && s.MetaMode != meta.Endpointing {
		return false
	}
	for _, value := range []string{s.MetaModel, s.DiarizationModel, s.ModelRevision, s.RuntimeVersion, s.Preset, s.MediaVersion, s.ParserVersion, s.ChunkingVersion} {
		if strings.TrimSpace(value) == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
			return false
		}
	}
	return true
}

// Options selects a private cache root and explicit reuse. Neither flag means a
// new generation, not implicit resume. Fresh never deletes prior generations, and
// Resume keeps selecting the previous one until the new one saves a result.
type Options struct {
	Dir           string
	Resume, Fresh bool
}

type artifact struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type normalized struct {
	Samples int64  `json:"samples"`
	SHA256  string `json:"sha256"`
}

func (n normalized) duration() time.Duration {
	return media.SampleDuration(n.Samples)
}

type completed struct {
	Response    artifact `json:"response"`
	AudioSHA256 string   `json:"audioSha256"`
	Attempts    int      `json:"attempts"`
}
type manifest struct {
	SchemaVersion int                  `json:"schemaVersion"`
	InputSHA256   string               `json:"inputSha256"`
	Settings      Settings             `json:"settings"`
	Media         *normalized          `json:"media"`
	RTTM          *artifact            `json:"rttm"`
	Plan          *[]chunking.Chunk    `json:"plan"`
	PlanSHA256    string               `json:"planSha256"`
	Completed     map[string]completed `json:"completed"`
	Complete      bool                 `json:"complete"`
}
type pointer struct {
	SchemaVersion int    `json:"schemaVersion"`
	Generation    string `json:"generation"`
}

type envelope struct {
	Manifest manifest `json:"manifest"`
	SHA256   string   `json:"manifestSha256"`
}
