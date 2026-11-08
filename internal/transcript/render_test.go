package transcript

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

func TestRenderFormatsAndTimestampModes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		format     Format
		timestamps Timestamps
		want       string
	}{
		{"defaults", "", "", "[00:00:00.000] **Alice:** Hello.\n\n[00:00:03.000] **Unknown speaker:** Yes.\n\n[00:00:14.000] **Speaker 2:** Next.\n"},
		{"markdown range", "markdown", "range", "[00:00:00.000 - 00:00:02.500] **Alice:** Hello.\n\n[00:00:03.000 - 00:00:04.000] **Unknown speaker:** Yes.\n\n[00:00:14.000 - 00:00:14.500] **Speaker 2:** Next.\n"},
		{"markdown none", "markdown", "none", "**Alice:** Hello.\n\n**Unknown speaker:** Yes.\n\n**Speaker 2:** Next.\n"},
		{"text start", "text", "start", "[00:00:00.000] Alice: Hello.\n[00:00:03.000] Unknown speaker: Yes.\n[00:00:14.000] Speaker 2: Next.\n"},
		{"text range", "text", "range", "[00:00:00.000 - 00:00:02.500] Alice: Hello.\n[00:00:03.000 - 00:00:04.000] Unknown speaker: Yes.\n[00:00:14.000 - 00:00:14.500] Speaker 2: Next.\n"},
		{"text none", "text", "none", "Alice: Hello.\nUnknown speaker: Yes.\nSpeaker 2: Next.\n"},
		{"srt", "srt", "", "1\n00:00:00,000 --> 00:00:02,500\nAlice: Hello.\n\n2\n00:00:03,000 --> 00:00:04,000\nUnknown speaker: Yes.\n\n3\n00:00:14,000 --> 00:00:14,500\nSpeaker 2: Next.\n"},
		{"srt ignores timestamp setting", "srt", "none", "1\n00:00:00,000 --> 00:00:02,500\nAlice: Hello.\n\n2\n00:00:03,000 --> 00:00:04,000\nUnknown speaker: Yes.\n\n3\n00:00:14,000 --> 00:00:14,500\nSpeaker 2: Next.\n"},
		{"vtt", "vtt", "", "WEBVTT\n\n00:00:00.000 --> 00:00:02.500\n<v Alice>Hello.\n\n00:00:03.000 --> 00:00:04.000\n<v Unknown speaker>Yes.\n\n00:00:14.000 --> 00:00:14.500\n<v Speaker 2>Next.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := example()
			before, _ := Encode(input)
			got, err := Render(context.Background(), input, RenderOptions{Format: tc.format, Timestamps: tc.timestamps})
			if err != nil || string(got) != tc.want {
				t.Fatalf("wrong rendering: %v\n%q", err, got)
			}
			again, err := Render(context.Background(), input, RenderOptions{Format: tc.format, Timestamps: tc.timestamps})
			if err != nil || !bytes.Equal(again, got) {
				t.Fatal("rendering is nondeterministic")
			}
			after, _ := Encode(input)
			if !bytes.Equal(before, after) {
				t.Fatal("rendering changed canonical data")
			}
		})
	}
}

func TestRenderJSONIgnoresPresentation(t *testing.T) {
	input := example()
	input.Turns[1].StartMS, input.Turns[1].EndMS = 3001, 3001
	input.SourceChunks[0].RawTurns[0].StartMS, input.SourceChunks[0].RawTurns[0].EndMS = 3001, 3001
	input.Turns[1].Attribution.Reason = InsufficientTiming
	want, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, timestamps := range append([]Timestamps{""}, TimestampStyles...) {
		got, err := Render(context.Background(), input, RenderOptions{Format: "json", Timestamps: timestamps})
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("presentation changed canonical JSON", err)
		}
		restored, err := Decode(got)
		if err != nil || !reflect.DeepEqual(restored, input) {
			t.Fatal("JSON lost canonical data", err)
		}
	}
	got, err := Render(context.Background(), input, RenderOptions{Format: "text", Timestamps: "range"})
	if err != nil || !strings.Contains(string(got), "[00:00:03.001 - 00:00:03.001] Unknown speaker") {
		t.Fatal("zero-duration text was omitted or retimed", err)
	}
}

func TestRenderEscapesMarkupAndTerminalControls(t *testing.T) {
	input := example()
	input.Speakers[0].Name = ptr("  A*[x]<b>&`\\\nB  ")
	input.Turns[0].Text = "\t[click](javascript:bad) ![image](x) <script>&amp;\n# Heading\r\n\x1b[31m\x00\u202e\u200e\nword\tword  "
	input.SourceChunks[0].RawTurns[1].Transcript = input.Turns[0].Text
	before, _ := Encode(input)
	markdown, err := Render(context.Background(), input, RenderOptions{Timestamps: "none"})
	if err != nil {
		t.Fatal(err)
	}
	want := "**A\\*\\[x\\]&lt;b&gt;&amp;\\`\\\\ B:** \\[click\\]\\(javascript:bad\\) \\!\\[image\\]\\(x\\) &lt;script&gt;&amp;amp; \\# Heading \\u001b\\[31m\\u0000\\u202e\\u200e word word\n"
	if !strings.HasPrefix(string(markdown), want) {
		t.Fatalf("markup escaping failed: %q", markdown)
	}
	plain, err := Render(context.Background(), input, RenderOptions{Format: "text", Timestamps: "none"})
	if err != nil || !strings.HasPrefix(string(plain), "A*[x]<b>&`\\ B: [click](javascript:bad) ![image](x) <script>&amp; # Heading \\u001b[31m\\u0000\\u202e\\u200e word word\n") {
		t.Fatalf("plain escaping failed: %v %q", err, plain)
	}
	for _, data := range [][]byte{markdown, plain} {
		if bytes.ContainsAny(data, "\r\t\x1b\x00\u202e\u200e") {
			t.Fatal("unsafe terminal control reached human output")
		}
	}
	after, _ := Encode(input)
	if !bytes.Equal(before, after) {
		t.Fatal("escaping altered canonical text/name")
	}
}

func TestRenderInvalidOptionsAndInputReturnNoDocument(t *testing.T) {
	for _, opts := range []RenderOptions{{Format: "bad"}, {Timestamps: "bad"}, {Format: "json", Timestamps: "bad"}} {
		if data, err := Render(context.Background(), example(), opts); data != nil || !errors.Is(err, ErrRenderOptions) {
			t.Fatal("invalid rendering options accepted")
		}
	}
	for _, format := range Formats {
		input := example()
		input.Turns[2].Source.MetaTurnID = 99
		if data, err := Render(context.Background(), input, RenderOptions{Format: format}); data != nil || !errors.Is(err, ErrInvalid) {
			t.Fatal("malformed transcript produced output")
		}
		if data, err := Render(context.Background(), nil, RenderOptions{Format: format}); data != nil || !errors.Is(err, ErrInvalid) {
			t.Fatal("nil transcript produced output")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if data, err := Render(ctx, example(), RenderOptions{}); data != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled renderer produced output")
	}
}

func TestRenderEmptyAndLongTimecodes(t *testing.T) {
	input := example()
	input.Turns = []Turn{}
	for _, format := range []Format{FormatMarkdown, FormatText} {
		if got, err := Render(context.Background(), input, RenderOptions{Format: format}); err != nil || len(got) != 0 {
			t.Fatal("empty canonical turns produced invented content")
		}
	}
	for _, tc := range []struct {
		ms   int64
		want string
	}{{0, "00:00:00.000"}, {3600001, "01:00:00.001"}, {360000000, "100:00:00.000"}, {math.MaxInt64, "2562047788015:12:55.807"}} {
		if got := timestamp(tc.ms); got != tc.want {
			t.Fatalf("timestamp rounded, wrapped or overflowed: %d %s", tc.ms, got)
		}
	}
}

type renderCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *renderCancelContext) Err() error {
	c.checks--
	if c.checks <= 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestRenderCancellationDiscardsPartlyBuiltDocument(t *testing.T) {
	input := example()
	input.Turns[0].Text = strings.Repeat("Long text. ", 1000)
	input.SourceChunks[0].RawTurns[1].Transcript = input.Turns[0].Text
	for _, format := range []Format{FormatMarkdown, FormatText} {
		ctx, cancel := context.WithCancel(context.Background())
		c := &renderCancelContext{Context: ctx, cancel: cancel, checks: 32}
		data, err := Render(c, input, RenderOptions{Format: format})
		cancel()
		if data != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation returned partly rendered text")
		}
	}
}

func FuzzRender(f *testing.F) {
	f.Add("Hello.", "Alice")
	f.Add("<script>\x1b[31m\u202e\n[x](bad)", "A*[x]")
	f.Fuzz(func(t *testing.T, text, name string) {
		if len(text) > 4096 || len(name) > 1024 {
			t.Skip()
		}
		input := example()
		input.Turns[0].Text = text
		input.SourceChunks[0].RawTurns[1].Transcript = text
		if name != "" {
			input.Speakers[0].Name = ptr(name)
		}
		before, validation := Encode(input)
		for _, format := range Formats {
			data, err := Render(context.Background(), input, RenderOptions{Format: format})
			if validation != nil {
				if data != nil || err == nil {
					t.Fatal("invalid transcript returned output")
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if format == "json" {
				if !bytes.Equal(data, before) {
					t.Fatal("JSON rendering changed canonical data")
				}
			} else {
				for _, r := range string(data) {
					if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)) {
						t.Fatal("unsafe terminal control reached human output")
					}
				}
			}
		}
		after, err := Encode(input)
		if validation == nil && (err != nil || !bytes.Equal(before, after)) {
			t.Fatal("rendering mutated canonical data")
		}
	})
}

func TestSubtitleEscapingEmptyAndLongTimes(t *testing.T) {
	input := example()
	name := "Ana <Lead> & Co"
	input.Speakers[0].Name = &name
	input.Turns[0].Text = "Use x < y -> z & done"
	input.SourceChunks[0].RawTurns[0].Transcript = input.Turns[0].Text
	vtt, err := Render(context.Background(), input, RenderOptions{Format: FormatVTT})
	if err != nil || !strings.Contains(string(vtt), "<v Ana &lt;Lead&gt; &amp; Co>Use x &lt; y -&gt; z &amp; done\n") {
		t.Fatalf("WebVTT markup was not escaped: %q, %v", vtt, err)
	}
	srt, err := Render(context.Background(), input, RenderOptions{Format: FormatSRT})
	if err != nil || !strings.Contains(string(srt), "Ana <Lead> & Co: Use x < y -> z & done\n") {
		t.Fatalf("SRT text changed: %q, %v", srt, err)
	}

	empty := example()
	empty.Turns = []Turn{}
	for format, want := range map[Format]string{FormatSRT: "", FormatVTT: "WEBVTT\n"} {
		if got, err := Render(context.Background(), empty, RenderOptions{Format: format}); err != nil || string(got) != want {
			t.Fatalf("empty %s = %q, %v", format, got, err)
		}
	}
	if got := cueTime(36_000_000+59*60_000+59_999, ','); got != "10:59:59,999" {
		t.Fatalf("long cue time = %q", got)
	}
	if got := cueTime(0, '.'); got != "00:00:00.000" {
		t.Fatalf("zero cue time = %q", got)
	}
}
