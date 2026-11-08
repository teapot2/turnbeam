package setup

import "io"

// ProgressStep is a fixed setup activity. Events never carry local paths, and
// Source is always the pinned URL, never a redirect target that may be signed.
type ProgressStep uint8

const (
	StepCheck ProgressStep = iota // verify an existing installation
	StepDownload
	StepVerify        // verify a newly staged installation
	StepCompatibility // run the native Metal and model check
)

// Progress reports Install activity. For StepDownload, Title describes the file,
// Source is its pinned HTTPS URL, and Done and Total count bytes.
type Progress struct {
	Step          ProgressStep
	Title, Source string
	Done, Total   int64
}

func (m Manager) report(p Progress) {
	if m.OnProgress != nil {
		m.OnProgress(p)
	}
}

func assetTitle(name string) string {
	switch name {
	case "runtime.tar.gz":
		return "NeMo-Speech.cpp v" + RuntimeVersion + " runtime (Apache-2.0)"
	case "model.gguf":
		return "Nemotron 3 diarization model, Q8_0 (OpenMDW-1.1)"
	case "MODEL-CARD.md":
		return "Nemotron 3 model card"
	case "MODEL-LICENSE":
		return "OpenMDW-1.1 model license"
	}
	return name
}

// countingReader reports cumulative bytes read for one downloading asset.
type countingReader struct {
	r     io.Reader
	done  int64
	event Progress
	emit  func(Progress)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.done += int64(n)
		c.event.Done = min(c.done, c.event.Total)
		c.emit(c.event)
	}
	return n, err
}
