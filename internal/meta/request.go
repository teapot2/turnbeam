package meta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"os"
	"sync/atomic"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
)

type uploadBody struct {
	io.Reader
	io.Closer
	read atomic.Int64
}

func (b *uploadBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read.Add(int64(n))
	return n, err
}

// Only framing lives in RAM. A SectionReader prevents file growth from expanding
// an already-checked request; truncation is an upload error, never silent success.
func upload(clip *media.Clip, model, mode string) (_ *uploadBody, _ string, _ int64, err error) {
	if clip == nil || clip.Prepared == nil || clip.StartSample < 0 || clip.EndSample <= clip.StartSample || clip.Samples <= 0 || clip.Samples > media.MaxChunkSamples || clip.EndSample-clip.StartSample != clip.Samples || clip.Duration != time.Duration(clip.Samples)*time.Second/media.SampleRate || clip.Bytes < 44+clip.Samples*2 || clip.Bytes >= media.MaxRequestBytes-media.RequestHeadroom {
		return nil, "", 0, media.ErrInvalidMedia
	}
	f, err := os.Open(clip.Path)
	if err != nil {
		return nil, "", 0, err
	}
	defer func() {
		if err != nil {
			f.Close()
		}
	}()
	stat, err := f.Stat()
	if err != nil {
		return nil, "", 0, err
	}
	if !stat.Mode().IsRegular() || stat.Size() != clip.Bytes {
		return nil, "", 0, media.ErrInvalidMedia
	}
	var framing bytes.Buffer
	w := multipart.NewWriter(&framing)
	part, err := w.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="request"`}, "Content-Type": {"application/json"}})
	if err != nil {
		return nil, "", 0, err
	}
	settings := struct {
		Model         string `json:"model"`
		AudioEncoding string `json:"audioEncoding"`
		Mode          string `json:"mode"`
	}{model, "WAV", mode}
	if err := json.NewEncoder(part).Encode(settings); err != nil {
		return nil, "", 0, err
	}
	if _, err := w.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="audio"; filename="chunk.wav"`}, "Content-Type": {"audio/wav"}}); err != nil {
		return nil, "", 0, err
	}
	prefix := framing.Len()
	if err := w.Close(); err != nil {
		return nil, "", 0, err
	}
	length := clip.Bytes + int64(framing.Len())
	if framing.Len() > media.RequestHeadroom || length >= media.MaxRequestBytes {
		return nil, "", 0, fmt.Errorf("%w: multipart body", media.ErrChunkSize)
	}
	data := framing.Bytes()
	return &uploadBody{Reader: io.MultiReader(bytes.NewReader(data[:prefix]), io.NewSectionReader(f, 0, clip.Bytes), bytes.NewReader(data[prefix:])), Closer: f}, w.FormDataContentType(), length, nil
}
