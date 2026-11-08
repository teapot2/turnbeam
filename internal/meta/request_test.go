package meta

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
)

func clipFile(t *testing.T, samples int64) *media.Clip {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private name.wav")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 44)
	copy(header, "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+samples*2))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], media.SampleRate)
	binary.LittleEndian.PutUint32(header[28:], media.SampleRate*2)
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(samples*2))
	if _, err := f.Write(header); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(44 + samples*2); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return &media.Clip{Prepared: &media.Prepared{Path: path, Samples: samples, Duration: time.Duration(samples) * time.Second / media.SampleRate}, StartSample: 48000, EndSample: 48000 + samples, Bytes: 44 + samples*2}
}

func verifyMultipart(t *testing.T, reader io.Reader, contentType, model, mode string, wavBytes int64) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(reader, params["boundary"])
	part, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if part.FormName() != "request" || part.Header.Get("Content-Type") != "application/json" {
		t.Fatal("wrong JSON part")
	}
	var settings map[string]string
	if err := json.NewDecoder(part).Decode(&settings); err != nil {
		t.Fatal(err)
	}
	if len(settings) != 3 || settings["model"] != model || settings["mode"] != mode || settings["audioEncoding"] != "WAV" {
		t.Fatalf("wrong request settings: %+v", settings)
	}
	part, err = mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if part.FormName() != "audio" || part.FileName() != "chunk.wav" || part.Header.Get("Content-Type") != "audio/wav" {
		t.Fatal("wrong/private audio part")
	}
	var header [12]byte
	if _, err := io.ReadFull(part, header[:]); err != nil || !bytes.Equal(header[:4], []byte("RIFF")) || string(header[8:]) != "WAVE" {
		t.Fatal("missing WAV bytes")
	}
	n, err := io.Copy(io.Discard, part)
	if err != nil || n+12 != wavBytes {
		t.Fatalf("wrong WAV length: %d, %v", n+12, err)
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatal("extra/unfinished part")
	}
}

func TestStreamingMultipart(t *testing.T) {
	clip := clipFile(t, media.MaxChunkSamples)
	for _, mode := range []string{Diarization, Endpointing} {
		body, contentType, length, err := upload(clip, Model, mode)
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		if length >= media.MaxRequestBytes || length-clip.Bytes > media.RequestHeadroom {
			t.Fatal("complete request exceeds budget")
		}
		if body.read.Load() != 0 {
			t.Fatal("audio eagerly buffered")
		}
		verifyMultipart(t, body, contentType, Model, mode, clip.Bytes)
		if body.read.Load() != length {
			t.Fatal("content length does not match serialized body")
		}
	}
}

func TestUploadRejectsInvalidOrChangedClip(t *testing.T) {
	clip := clipFile(t, 24000)
	for _, c := range []*media.Clip{nil, {}, {Prepared: clip.Prepared, EndSample: 24001, Bytes: clip.Bytes}, {Prepared: clip.Prepared, EndSample: 24000, Bytes: media.MaxRequestBytes}} {
		if body, _, _, err := upload(c, Model, Diarization); body != nil || err == nil {
			t.Fatal("invalid clip accepted")
		}
	}
	if err := os.Truncate(clip.Path, clip.Bytes-1); err != nil {
		t.Fatal(err)
	}
	if body, _, _, err := upload(clip, Model, Diarization); body != nil || !errors.Is(err, media.ErrInvalidMedia) {
		t.Fatal("changed clip accepted")
	}
	clip = clipFile(t, 24000)
	body, _, length, err := upload(clip, Model, Diarization)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if err := os.Truncate(clip.Path, 44); err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, body)
	if n == length {
		t.Fatal("truncated audio masqueraded as complete upload")
	}
	clip = clipFile(t, 24000)
	body, _, length, err = upload(clip, Model, Diarization)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if err := os.Truncate(clip.Path, clip.Bytes+4096); err != nil {
		t.Fatal(err)
	}
	if n, err := io.Copy(io.Discard, body); err != nil || n != length {
		t.Fatal("file growth changed checked request length")
	}
}
