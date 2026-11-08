package meta

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalCaptures(t *testing.T) {
	if os.Getenv("TURNBEAM_TEST_LOCAL_FIXTURES") != "1" {
		t.Skip("set TURNBEAM_TEST_LOCAL_FIXTURES=1 to check local API captures")
	}
	for _, name := range []string{"short-diarization", "short-endpointing", "long-diarization", "long-endpointing", "auth-missing", "auth-invalid", "oversized", "oversized-trailing"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("..", "..", "testdata", "meta", "real", name)
			data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Status    int               `json:"status"`
				Artifacts map[string]string `json:"artifacts"`
				Sample    struct {
					DurationMS int64 `json:"durationMs"`
				} `json:"sample"`
				Validation struct {
					TurnCount int `json:"turnCount"`
				} `json:"validation"`
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"request.json", "response.json"} {
				content, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(content)
				if hex.EncodeToString(sum[:]) != manifest.Artifacts[file] {
					t.Fatal("capture checksum mismatch")
				}
			}
			response, err := os.ReadFile(filepath.Join(dir, "response.json"))
			if err != nil {
				t.Fatal(err)
			}
			if manifest.Status == 200 {
				r, err := decodeResponse(response, manifest.Sample.DurationMS)
				if err != nil || len(r.Turns) != manifest.Validation.TurnCount {
					t.Fatalf("captured success contract rejected: %v", err)
				}
				if manifest.Sample.DurationMS == 15000 && r.AudioDurationMS != 15040 {
					t.Fatal("API duration discrepancy lost")
				}
			} else {
				e := &Error{Kind: ErrHTTP, StatusCode: manifest.Status}
				e.readTaxonomy(response, "unused-credential")
				if e.Type == "" || manifest.Status != 400 && manifest.Status != 401 {
					t.Fatal("captured HTTP error contract rejected")
				}
			}
		})
	}
}
