package nativeenv

import (
	"reflect"
	"testing"
)

func TestSanitize(t *testing.T) {
	input := []string{
		"PATH=/fixture/bin", "META_API_KEY=secret", "DYLD_LIBRARY_PATH=untrusted", "LD_PRELOAD=untrusted",
		"NEMO_SPEECH_CONFIG=untrusted", "GGML_BACKEND_PATH=untrusted", "LLAMA_CONFIG=untrusted", "HOME=/fixture/home",
	}
	want := []string{"PATH=/fixture/bin", "HOME=/fixture/home"}
	before := append([]string(nil), input...)
	got := Sanitize(input)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(input, before) {
		t.Fatalf("environment policy or input changed: %v", got)
	}
}
