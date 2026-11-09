package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
)

const (
	defaultRecordings = 20
	maxRecordings     = 50
	// maxSearchDepth bounds a recursive search below the chosen directory.
	maxSearchDepth = 3
	// maxScanned bounds the directory entries one search visits.
	maxScanned = 50_000
	probeWait  = 10 * time.Second
)

// mediaExtensions are the file types FFmpeg is likely to decode as a meeting.
var mediaExtensions = map[string]bool{
	".mp4": true, ".m4a": true, ".m4v": true, ".mov": true, ".mp3": true, ".wav": true, ".aac": true,
	".flac": true, ".ogg": true, ".oga": true, ".opus": true, ".webm": true, ".mkv": true, ".aiff": true,
	".aif": true, ".wma": true, ".wmv": true, ".avi": true, ".caf": true, ".amr": true, ".3gp": true,
	".mpg": true, ".mpeg": true,
}

// defaultSearchFolders are searched under the home directory when the server
// has only its default root and a call names no directory.
var defaultSearchFolders = []string{"Downloads", "Desktop", "Documents", "Movies"}

type listArgs struct {
	Directory string `json:"directory"`
	Since     string `json:"since"`
	Until     string `json:"until"`
	Recursive *bool  `json:"recursive"`
	Limit     int    `json:"limit"`
}

type recording struct {
	Path            string  `json:"path"`
	Name            string  `json:"name"`
	Modified        string  `json:"modified"`
	SizeBytes       int64   `json:"size_bytes"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Note            string  `json:"note,omitempty"`
	Transcript      string  `json:"transcript,omitempty"`
	modified        time.Time
}

type listing struct {
	Directories []string    `json:"directories"`
	Recordings  []recording `json:"recordings"`
	// Truncated means more recordings matched than the limit allowed.
	Truncated bool `json:"truncated,omitempty"`
	// Partial means a search stopped at its entry budget before finishing.
	Partial bool `json:"partial,omitempty"`
}

func listRecordings(ctx context.Context, s *Server, _ *call, raw json.RawMessage) (*toolResult, error) {
	var args listArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	limit := args.Limit
	switch {
	case limit == 0:
		limit = defaultRecordings
	case limit < 1 || limit > maxRecordings:
		return nil, argumentError("limit must be between 1 and %d", maxRecordings)
	}
	since, hasSince, err := parseTime(args.Since, false)
	if err != nil {
		return nil, argumentError("since: %v", err)
	}
	until, hasUntil, err := parseTime(args.Until, true)
	if err != nil {
		return nil, argumentError("until: %v", err)
	}
	if hasSince && hasUntil && until.Before(since) {
		return nil, argumentError("until must not be before since")
	}
	dirs, err := s.searchDirs(args.Directory)
	if err != nil {
		return nil, err
	}
	recursive := args.Recursive == nil || *args.Recursive
	out := listing{Directories: dirs, Recordings: []recording{}}
	for _, dir := range dirs {
		found, partial, err := scan(ctx, dir, recursive, func(mod time.Time) bool {
			return (!hasSince || !mod.Before(since)) && (!hasUntil || !mod.After(until))
		})
		if err != nil {
			return nil, err
		}
		out.Partial = out.Partial || partial
		out.Recordings = append(out.Recordings, found...)
	}
	slices.SortStableFunc(out.Recordings, func(a, b recording) int {
		if c := b.modified.Compare(a.modified); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	if len(out.Recordings) > limit {
		out.Recordings, out.Truncated = out.Recordings[:limit], true
	}
	for i := range out.Recordings {
		s.annotate(ctx, &out.Recordings[i])
	}
	return result(out)
}

// searchDirs resolves the requested directory, or picks the defaults.
func (s *Server) searchDirs(directory string) ([]string, error) {
	if directory != "" {
		dir, err := s.roots.resolve(directory, true)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(dir)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, argumentError("directory is not a folder: %s", directory)
		}
		return []string{dir}, nil
	}
	home, err := os.UserHomeDir()
	if err == nil {
		home, err = filepath.EvalSymlinks(home)
	}
	if err != nil || len(s.roots) != 1 || s.roots[0] != home {
		return slices.Clone(s.roots), nil
	}
	var dirs []string
	for _, name := range defaultSearchFolders {
		dir := filepath.Join(home, name)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		dirs = []string{home}
	}
	return dirs, nil
}

// scan lists media files under dir whose modification time passes keep.
// Hidden entries and symbolic links are skipped; the search stops at its
// entry budget and reports that it did.
func scan(ctx context.Context, dir string, recursive bool, keep func(time.Time) bool) (found []recording, partial bool, err error) {
	scanned := 0
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path == dir {
				return walkErr
			}
			return nil // Unreadable entries are skipped, not fatal.
		}
		if scanned++; scanned > maxScanned {
			partial = true
			return fs.SkipAll
		}
		hidden := path != dir && strings.HasPrefix(d.Name(), ".")
		if d.IsDir() {
			if path == dir {
				return nil
			}
			rel, _ := filepath.Rel(dir, path)
			if hidden || !recursive || strings.Count(rel, string(filepath.Separator))+1 > maxSearchDepth {
				return fs.SkipDir
			}
			return nil
		}
		if hidden || !d.Type().IsRegular() || !mediaExtensions[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || !keep(info.ModTime()) {
			return nil
		}
		found = append(found, recording{Path: path, Name: d.Name(), Modified: info.ModTime().Format(time.RFC3339), SizeBytes: info.Size(), modified: info.ModTime()})
		return nil
	})
	if errors.Is(err, fs.SkipAll) {
		err = nil
	}
	return found, partial, err
}

// annotate adds a recording's duration and existing transcript, if any.
func (s *Server) annotate(ctx context.Context, r *recording) {
	if sidecar := sidecarFor(r.Path); fileExists(sidecar) {
		r.Transcript = sidecar
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	duration, err := s.Probe(probeCtx, r.Path)
	switch {
	case errors.Is(err, media.ErrNoAudio):
		r.Note = "no audio stream"
	case err != nil:
		r.Note = "duration unavailable"
	default:
		r.DurationSeconds = roundSeconds(duration)
	}
}

func sidecarFor(path string) string {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return filepath.Join(filepath.Dir(path), stem+".transcript.json")
}

func fileExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func roundSeconds(d time.Duration) float64 {
	return float64(d.Round(100*time.Millisecond)) / float64(time.Second)
}

// parseTime accepts an RFC 3339 time or a local calendar date; a date used as
// an upper bound covers its whole day.
func parseTime(value string, endOfDay bool) (time.Time, bool, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, true, nil
	}
	t, err := time.ParseInLocation("2006-01-02", value, time.Local)
	if err != nil {
		return time.Time{}, false, errors.New("use YYYY-MM-DD or an RFC 3339 time")
	}
	if endOfDay {
		t = t.AddDate(0, 0, 1).Add(-time.Nanosecond)
	}
	return t, true, nil
}
