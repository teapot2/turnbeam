package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/testutil"
)

const nativeDoctor = `{"version":"0.3.0","accelerator_available":true,"accelerator_compiled":true,"driver_runtime_compatible":true,"features":{"backend_metal":true,"diarization":true},"devices":[{"name":"MTL0","description":"Apple test GPU","type":"gpu"}]}`
const nativeModel = `{"architecture":"sortformer","role":"diarization","runtime_compatible":true,"errors":[]}`

func TestDefaultDirPreservesInstallationLocation(t *testing.T) {
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DefaultDir()
	if err != nil || got != filepath.Join(base, "turnbeam") {
		t.Fatalf("default installation location changed: %q, %v", got, err)
	}
}

type member struct {
	name, body, link string
	kind             byte
	mode             int64
}

func archiveBytes(t *testing.T, members []member) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		h := &tar.Header{Name: m.name, Typeflag: m.kind, Mode: m.mode, Linkname: m.link, Size: int64(len(m.body))}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func validArchive(t *testing.T, prefix string) []byte {
	return archiveBytes(t, []member{
		{name: prefix + "/", kind: tar.TypeDir, mode: 0755},
		{name: prefix + "/bin/", kind: tar.TypeDir, mode: 0755},
		{name: prefix + "/lib/", kind: tar.TypeDir, mode: 0755},
		{name: prefix + "/bin/nemo-speech", body: "verified executable", kind: tar.TypeReg, mode: 0755},
		{name: prefix + "/lib/native.1.dylib", body: "verified library", kind: tar.TypeReg, mode: 0755},
		{name: prefix + "/lib/native.dylib", link: "native.1.dylib", kind: tar.TypeSymlink, mode: 0777},
		{name: prefix + "/NOTICE", body: "publisher notices", kind: tar.TypeReg, mode: 0644},
	})
}

type fixture struct {
	manager Manager
	hits    atomic.Int64
	native  atomic.Int64
	bodies  map[string][]byte
}

func newFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	f := &fixture{bodies: map[string][]byte{}}
	c := pinnedCatalog()
	contents := [][]byte{validArchive(t, c.prefix), []byte("GGUF fixture"), []byte("model attribution"), []byte("model license")}
	for i := range c.assets {
		a := &c.assets[i]
		body := contents[i]
		sum := sha256.Sum256(body)
		a.sha256, a.size = hex.EncodeToString(sum[:]), int64(len(body))
		f.bodies["/"+a.name] = body
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		body, ok := f.bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	for i := range c.assets {
		c.assets[i].url = server.URL + "/" + c.assets[i].name
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.manager = Manager{
		Dir: filepath.Join(t.TempDir(), "managed"), Client: server.Client(), pins: &c,
		system: func() (string, string) { return "darwin", "arm64" },
		command: func(ctx context.Context, tool string, args ...string) *exec.Cmd {
			if filepath.Base(tool) == "nemo-speech" {
				f.native.Add(1)
			}
			cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestSetupProcess$", "--", tool}, args...)...)
			cmd.Env = append(os.Environ(), "TURNBEAM_SETUP_HELPER=1", "TURNBEAM_SETUP_MODE="+mode, "GORACE=atexit_sleep_ms=0")
			return cmd
		},
	}
	return f
}

func TestSetupProcess(t *testing.T) {
	if os.Getenv("TURNBEAM_SETUP_HELPER") != "1" {
		return
	}
	args := os.Args
	for args[0] != "--" {
		args = args[1:]
	}
	tool, args := filepath.Base(args[1]), args[2:]
	mode := os.Getenv("TURNBEAM_SETUP_MODE")
	if tool == "ffmpeg" || tool == "ffprobe" {
		if mode == "missing-ffmpeg" && tool == "ffmpeg" {
			os.Exit(1)
		}
		fmt.Println(tool, "version test")
		os.Exit(0)
	}
	data := nativeDoctor
	if len(args) > 1 && args[1] == "model" {
		data = nativeModel
		if mode == "mismatch" {
			data = strings.Replace(data, `"runtime_compatible":true`, `"runtime_compatible":false`, 1)
		}
	} else {
		switch mode {
		case "no-metal":
			data = strings.Replace(data, `"backend_metal":true`, `"backend_metal":false`, 1)
		case "sandbox-gpu":
			data = strings.Replace(data, `"name":"MTL0"`, `"name":""`, 1)
		case "wrong-version":
			data = strings.Replace(data, `"version":"0.3.0"`, `"version":"0.0.0"`, 1)
		case "untrusted-description":
			data = strings.Replace(data, "Apple test GPU", "private-native-value", 1)
		case "malformed":
			data = "{"
		case "oversized":
			data = strings.Repeat("x", 65537)
		case "exit":
			fmt.Fprintln(os.Stderr, "sensitive native diagnostics")
			os.Exit(1)
		case "hang":
			if err := os.WriteFile(os.Getenv("TURNBEAM_SETUP_READY"), []byte("ready"), 0600); err != nil {
				os.Exit(2)
			}
			time.Sleep(time.Hour)
		}
	}
	fmt.Print(data)
	os.Exit(0)
}

func TestNativeEnvironmentProcess(t *testing.T) {
	if os.Getenv("TURNBEAM_SETUP_ENV_HELPER") != "1" {
		return
	}
	for _, name := range []string{"META_API_KEY", "DYLD_LIBRARY_PATH", "NEMO_SPEECH_CONFIG", "GGML_BACKEND_PATH"} {
		if os.Getenv(name) != "" {
			os.Exit(1)
		}
	}
	fmt.Print("clean")
	os.Exit(0)
}

func TestNativeEnvironmentExcludesCredentialsAndOverrides(t *testing.T) {
	t.Setenv("TURNBEAM_SETUP_ENV_HELPER", "1")
	t.Setenv("META_API_KEY", "test-only-secret")
	t.Setenv("DYLD_LIBRARY_PATH", "/untrusted-test-path")
	t.Setenv("NEMO_SPEECH_CONFIG", "untrusted-config")
	t.Setenv("GGML_BACKEND_PATH", "/untrusted-backend-path")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := Manager{command: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, exe, "-test.run=^TestNativeEnvironmentProcess$")
	}}
	output, err := m.run(context.Background(), "unused")
	if err != nil || string(output) != "clean" {
		t.Fatalf("unsafe native environment: %v", err)
	}
}

func TestInstallVerifyDoctorAndReuse(t *testing.T) {
	f := newFixture(t, "")
	installed, err := f.manager.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 4 || f.native.Load() != 2 {
		t.Fatalf("wrong download/check count: %d, %d", f.hits.Load(), f.native.Load())
	}
	got, err := f.manager.Verify(context.Background())
	if err != nil || got != installed {
		t.Fatalf("installed pair did not verify: %+v, %v", got, err)
	}
	again, err := f.manager.Install(context.Background())
	if err != nil || again != installed || f.hits.Load() != 4 {
		t.Fatalf("idempotent install downloaded or changed data: %+v, %v", again, err)
	}
	checks, err := f.manager.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		if !check.OK {
			t.Fatalf("doctor failed: %+v", check)
		}
	}
	if f.hits.Load() != 4 {
		t.Fatal("doctor performed network I/O")
	}
	for _, name := range []string{"runtime.tar.gz", "model.gguf", "MODEL-LICENSE", "MODEL-CARD.md"} {
		stat, err := os.Stat(filepath.Join(installed.Directory, name))
		if err != nil || stat.Mode().Perm() != 0600 {
			t.Fatalf("missing/private asset %s: %v", name, err)
		}
	}
	assertNoStaging(t, f.manager.Dir)
}

func TestReadyVerifiesBeforeNativeChecks(t *testing.T) {
	f := newFixture(t, "")
	installed, err := f.manager.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ready, err := f.manager.Ready(context.Background())
	if err != nil || ready != installed || f.hits.Load() != 4 {
		t.Fatalf("ready resolution changed installation or downloaded files: %+v, %v", ready, err)
	}
	before := f.native.Load()
	if err := os.WriteFile(installed.Runtime, []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Ready(context.Background()); !errors.Is(err, ErrIntegrity) || f.native.Load() != before {
		t.Fatalf("unverified runtime was resolved or executed: %v", err)
	}
}

func TestRepairAndIntegrityBeforeExecution(t *testing.T) {
	f := newFixture(t, "")
	installed, err := f.manager.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed.Runtime, []byte("tampered executable"), 0700); err != nil {
		t.Fatal(err)
	}
	before := f.native.Load()
	if _, err := f.manager.Verify(context.Background()); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("modified executable verified: %v", err)
	}
	if _, err := f.manager.Doctor(context.Background()); !errors.Is(err, ErrDoctorFailed) || !errors.Is(err, ErrIntegrity) {
		t.Fatalf("doctor did not retain integrity failure: %v", err)
	}
	if f.native.Load() != before {
		t.Fatal("unverified runtime was executed")
	}
	repaired, err := f.manager.Install(context.Background())
	if err != nil || repaired.Directory == installed.Directory || f.hits.Load() != 4 {
		t.Fatalf("repair did not reuse trusted downloads: %+v, %v", repaired, err)
	}
	if _, err := os.Stat(installed.Directory); err != nil {
		t.Fatal("previous generation was removed")
	}
	if err := os.WriteFile(repaired.Model, []byte("bad model"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Install(context.Background()); err != nil || f.hits.Load() != 5 {
		t.Fatalf("model repair did not download exactly the damaged asset: %v (%d requests)", err, f.hits.Load())
	}
	assertNoStaging(t, f.manager.Dir)
}

func TestInstalledSymlinksAndUnexpectedFiles(t *testing.T) {
	for _, modification := range []string{"extra library", "link escape", "library content", "archive content", "bad pointer", "public root"} {
		t.Run(modification, func(t *testing.T) {
			f := newFixture(t, "")
			installed, err := f.manager.Install(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch modification {
			case "extra library":
				err = os.WriteFile(filepath.Join(installed.Directory, "runtime/lib/extra.dylib"), []byte("extra"), 0600)
			case "link escape":
				link := filepath.Join(installed.Directory, "runtime/lib/native.dylib")
				if err = os.Remove(link); err == nil {
					err = os.Symlink("/tmp/outside", link)
				}
			case "library content":
				err = os.WriteFile(filepath.Join(installed.Directory, "runtime/lib/native.1.dylib"), []byte("changed"), 0700)
			case "archive content":
				err = os.WriteFile(filepath.Join(installed.Directory, "runtime.tar.gz"), []byte("changed"), 0600)
			case "bad pointer":
				err = os.WriteFile(filepath.Join(f.manager.Dir, "current.json"), []byte(`{"schemaVersion":1,"generation":"../outside"}`), 0600)
			case "public root":
				err = os.Chmod(f.manager.Dir, 0777)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.manager.Verify(context.Background())
			if err == nil {
				t.Fatal("modified installation was accepted")
			}
			if modification == "public root" && !errors.Is(err, ErrUnsafeDirectory) {
				t.Fatal("public installation directory not reported as unsafe", err)
			}
		})
	}
}

func TestNativeCompatibilityFailuresRollBack(t *testing.T) {
	for _, mode := range []string{"no-metal", "sandbox-gpu", "wrong-version", "malformed", "oversized", "mismatch", "exit"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, mode)
			if _, err := f.manager.Install(context.Background()); err == nil || strings.Contains(err.Error(), "sensitive native diagnostics") {
				t.Fatalf("bad runtime/model was activated: %v", err)
			}
			if _, err := os.Stat(filepath.Join(f.manager.Dir, "current.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial install has a current pointer: %v", err)
			}
			assertNoStaging(t, f.manager.Dir)
		})
	}
}

func TestFailedUpgradeKeepsPreviousPointer(t *testing.T) {
	f := newFixture(t, "")
	installed, err := f.manager.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pins := f.manager.catalog()
	pins.assets = append([]artifact(nil), pins.assets...)
	pins.assets[1].url += "-missing"
	pins.assets[1].sha256 = strings.Repeat("0", 64)
	upgrade := f.manager
	upgrade.pins = &pins
	if _, err := upgrade.Install(context.Background()); err == nil {
		t.Fatal("missing new asset unexpectedly installed")
	}
	if got, err := f.manager.Verify(context.Background()); err != nil || got != installed {
		t.Fatalf("failed upgrade damaged previous installation: %+v, %v", got, err)
	}
	assertNoStaging(t, f.manager.Dir)
}

func TestFailedActivationDoesNotPublishInstallation(t *testing.T) {
	f := newFixture(t, "")
	if err := os.MkdirAll(filepath.Join(f.manager.Dir, "current.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Install(context.Background()); err == nil {
		t.Fatal("failed pointer write was reported as complete")
	}
	entries, err := os.ReadDir(f.manager.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "current.json" && entry.Name() != "setup.lock" {
			t.Fatalf("uncommitted installation remains: %s", entry.Name())
		}
	}
}

func TestDoctorMissingDependency(t *testing.T) {
	f := newFixture(t, "missing-ffmpeg")
	if _, err := f.manager.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	checks, err := f.manager.Doctor(context.Background())
	var exit *exec.ExitError
	if !errors.Is(err, ErrDoctorFailed) || !errors.As(err, &exit) {
		t.Fatalf("doctor lost dependency failure: %v", err)
	}
	for _, check := range checks {
		if check.Name == "FFmpeg" && !check.OK && strings.Contains(check.Fix, "install FFmpeg") {
			return
		}
	}
	t.Fatal("doctor did not provide a dependency repair action")
}

func TestNativeCancellationRemovesStagingAndReleasesLock(t *testing.T) {
	f := newFixture(t, "hang")
	ready := filepath.Join(t.TempDir(), "ready")
	command := f.manager.command
	f.manager.command = func(ctx context.Context, tool string, args ...string) *exec.Cmd {
		cmd := command(ctx, tool, args...)
		cmd.Env = append(cmd.Env, "TURNBEAM_SETUP_READY="+ready)
		return cmd
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.manager.Install(ctx); done <- err }()
	testutil.WaitReady(t, ready, done, cancel)
	cancel()
	err := testutil.WaitError(t, done)
	var exit *exec.ExitError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &exit) {
		t.Fatalf("native check was not canceled and waited: %v", err)
	}
	assertNoStaging(t, f.manager.Dir)
	root, err := os.OpenRoot(f.manager.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := lockInstall(root)
	if err != nil {
		t.Fatalf("canceled setup left its lock held: %v", err)
	}
	lock.Close()
}

func TestPlatformMissingInstallAndLock(t *testing.T) {
	f := newFixture(t, "")
	f.manager.system = func() (string, string) { return "linux", "amd64" }
	if _, err := f.manager.Install(context.Background()); !errors.Is(err, ErrUnsupportedPlatform) || f.hits.Load() != 0 {
		t.Fatalf("unsupported platform was not rejected before download: %v", err)
	}
	f.manager.system = func() (string, string) { return "darwin", "arm64" }
	if _, err := f.manager.Doctor(context.Background()); !errors.Is(err, ErrDoctorFailed) {
		t.Fatalf("missing installation was accepted: %v", err)
	}
	if err := os.MkdirAll(f.manager.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(f.manager.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := lockInstall(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Install(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent installation was accepted: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.manager.Install(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUnsafeArchives(t *testing.T) {
	prefix := pinnedCatalog().prefix
	for name, bad := range map[string]member{
		"parent traversal": {name: prefix + "/../escape", kind: tar.TypeReg, body: "bad"},
		"absolute":         {name: "/outside/file", kind: tar.TypeReg, body: "bad"},
		"symlink escape":   {name: prefix + "/link", kind: tar.TypeSymlink, link: "../outside"},
		"absolute symlink": {name: prefix + "/link", kind: tar.TypeSymlink, link: "/outside"},
		"hard link":        {name: prefix + "/link", kind: tar.TypeLink, link: "file"},
		"FIFO":             {name: prefix + "/pipe", kind: tar.TypeFifo},
		"backslash":        {name: prefix + `/..\outside`, kind: tar.TypeReg, body: "bad"},
	} {
		t.Run(name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			file, err := os.CreateTemp(t.TempDir(), "archive")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.Write(archiveBytes(t, []member{bad})); err != nil {
				t.Fatal(err)
			}
			file.Seek(0, io.SeekStart)
			if _, err := readArchive(context.Background(), file, prefix, root); err == nil {
				t.Fatal("unsafe archive was extracted")
			}
		})
	}
}

func TestDownloadPoliciesAndBounds(t *testing.T) {
	for _, mode := range []string{"http", "redirect", "status", "length", "checksum", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			var insecureHits atomic.Int64
			insecure := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { insecureHits.Add(1) }))
			defer insecure.Close()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, insecure.URL, http.StatusFound)
				case "status":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "length":
					io.WriteString(w, "too long")
				case "checksum":
					io.WriteString(w, "evil")
				case "truncated":
					w.Header().Set("Content-Length", "4")
					io.WriteString(w, "x")
				}
			}))
			defer server.Close()
			a := artifact{name: "asset", url: server.URL, size: 4, sha256: strings.Repeat("0", 64)}
			if mode == "http" {
				a.url = insecure.URL
			}
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			err = (Manager{Client: server.Client()}).download(context.Background(), root, a)
			if err == nil {
				t.Fatal("unsafe or malformed download accepted")
			}
			if mode == "status" && !errors.Is(err, ErrDownload) {
				t.Fatal("HTTP failure is not a download error", err)
			}
			if insecureHits.Load() != 0 {
				t.Fatal("download used insecure HTTP")
			}
		})
	}
}

func TestDownloadCancellationCleansUp(t *testing.T) {
	f := newFixture(t, "")
	ready := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		close(ready)
		<-r.Context().Done()
	}))
	defer server.Close()
	c := f.manager.catalog()
	c.assets[0].url, c.assets[0].size = server.URL, 1000
	f.manager.Client, f.manager.pins = server.Client(), &c
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.manager.Install(ctx); done <- err }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("download failed before cancellation: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("download did not start")
	}
	cancel()
	if err := testutil.WaitError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
	assertNoStaging(t, f.manager.Dir)
}

func assertNoStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".stage-") || strings.HasPrefix(entry.Name(), ".current-") {
			t.Fatalf("partial installation remains: %s", entry.Name())
		}
	}
}

func TestInstallReportsProgressWithoutURLs(t *testing.T) {
	f := newFixture(t, "")
	var events []Progress
	f.manager.OnProgress = func(p Progress) { events = append(events, p) }
	if _, err := f.manager.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	pinned := map[string]string{}
	for _, a := range f.manager.pins.assets {
		pinned[assetTitle(a.name)] = a.url
	}
	started, finished := map[string]bool{}, map[string]bool{}
	var steps []ProgressStep
	for _, e := range events {
		if e.Done < 0 || e.Done > e.Total {
			t.Fatalf("inconsistent event: %+v", e)
		}
		if e.Step != StepDownload {
			steps = append(steps, e.Step)
			continue
		}
		if pinned[e.Title] != e.Source {
			t.Fatalf("download source is not the pinned URL: %+v", e)
		}
		if e.Done == 0 {
			started[e.Title] = true
		}
		if e.Done == e.Total && started[e.Title] {
			finished[e.Title] = true
		}
	}
	for _, title := range []string{"NeMo-Speech.cpp v" + RuntimeVersion + " runtime (Apache-2.0)", "Nemotron 3 diarization model, Q8_0 (OpenMDW-1.1)", "Nemotron 3 model card", "OpenMDW-1.1 model license"} {
		if !finished[title] {
			t.Fatalf("download of %q was not reported from start to finish: %+v", title, events)
		}
	}
	if !reflect.DeepEqual(steps, []ProgressStep{StepVerify, StepCompatibility}) {
		t.Fatalf("unexpected steps for a fresh install: %v", steps)
	}

	events = nil
	if _, err := f.manager.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []Progress{{Step: StepCheck}, {Step: StepCompatibility}}) {
		t.Fatalf("a verified installation should only be checked: %+v", events)
	}
}
