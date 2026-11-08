package setup

import (
	"context"
	"strings"
	"testing"
)

func TestDoctorDoesNotEchoNativeMetadata(t *testing.T) {
	f := newFixture(t, "untrusted-description")
	if _, err := f.manager.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	checks, err := f.manager.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		if strings.Contains(check.Message, "private-native-value") {
			t.Fatal("doctor echoed arbitrary native metadata")
		}
	}
}

func TestDoctorNamesAppleChipsAndToolVersions(t *testing.T) {
	if !appleChip.MatchString("Apple M5") || !appleChip.MatchString("Apple M3 Pro") || appleChip.MatchString("Apple M3 Pro; rm -rf") {
		t.Fatal("chip allowlist is wrong")
	}
	for _, tc := range []struct {
		data, tool, want string
		ok               bool
	}{
		{"ffmpeg version 9.0.2 Copyright (c) 2000-2026\nbuilt with clang", "ffmpeg", "9.0.2", true},
		{"ffprobe version n7.1-12-g1234 Copyright", "ffprobe", "n7.1-12-g1234", true},
		{"ffmpeg version \x1b[31mred Copyright", "ffmpeg", "", false},
		{"something else", "ffmpeg", "", false},
	} {
		if got, ok := toolVersion([]byte(tc.data), tc.tool); got != tc.want || ok != tc.ok {
			t.Fatalf("toolVersion(%q) = %q, %v", tc.data, got, ok)
		}
	}
}
