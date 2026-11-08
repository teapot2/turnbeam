package transcript

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

var ErrRenderOptions = errors.New("invalid transcript rendering options")

// Format selects the rendered document type.
type Format string

const (
	FormatMarkdown Format = "markdown"
	FormatText     Format = "text"
	FormatJSON     Format = "json"
	FormatSRT      Format = "srt" // SubRip subtitles
	FormatVTT      Format = "vtt" // WebVTT subtitles
)

// Timestamps selects how human formats show turn times.
type Timestamps string

const (
	TimestampsStart Timestamps = "start"
	TimestampsRange Timestamps = "range"
	TimestampsNone  Timestamps = "none"
)

// Formats and TimestampStyles list the accepted values in display order.
var (
	Formats         = []Format{FormatMarkdown, FormatText, FormatJSON, FormatSRT, FormatVTT}
	TimestampStyles = []Timestamps{TimestampsStart, TimestampsRange, TimestampsNone}
)

// RenderOptions controls presentation only. Empty values select the first
// entry of Formats and TimestampStyles.
type RenderOptions struct {
	Format     Format
	Timestamps Timestamps
}

// Validate checks presentation choices without requiring transcript data or I/O.
func (o RenderOptions) Validate() error {
	if o.Format != "" && !slices.Contains(Formats, o.Format) || o.Timestamps != "" && !slices.Contains(TimestampStyles, o.Timestamps) {
		return ErrRenderOptions
	}
	return nil
}

// Render returns one complete, deterministic document without changing its input.
// Human formats fold whitespace and visibly escape terminal/bidi controls. JSON
// uses canonical Encode unchanged, and subtitles always show each turn's start
// and end; neither uses the timestamp presentation setting.
func Render(ctx context.Context, input *Transcript, opts RenderOptions) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Format == "" {
		opts.Format = FormatMarkdown
	}
	if opts.Timestamps == "" {
		opts.Timestamps = TimestampsStart
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if opts.Format == FormatJSON {
		data, err := Encode(input)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return data, nil
	}
	if err := Validate(input); err != nil {
		return nil, err
	}
	if opts.Format == FormatSRT || opts.Format == FormatVTT {
		return subtitles(ctx, input, opts.Format)
	}
	markdown := opts.Format == FormatMarkdown
	labels := make(map[string]string, len(input.Speakers))
	for _, speaker := range input.Speakers {
		name := fmt.Sprintf("Speaker %d", speaker.Index)
		if speaker.Name != nil {
			name = *speaker.Name
		}
		label, err := displayText(ctx, name, markdown)
		if err != nil {
			return nil, err
		}
		labels[speaker.ID] = label
	}
	var out strings.Builder
	for i, turn := range input.Turns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if markdown && i != 0 {
			out.WriteByte('\n')
		}
		if opts.Timestamps != TimestampsNone {
			out.WriteByte('[')
			out.WriteString(timestamp(turn.StartMS))
			if opts.Timestamps == TimestampsRange {
				out.WriteString(" - ")
				out.WriteString(timestamp(turn.EndMS))
			}
			out.WriteString("] ")
		}
		label := "Unknown speaker"
		if turn.SpeakerID != nil {
			label = labels[*turn.SpeakerID]
		}
		if markdown {
			out.WriteString("**" + label + ":** ")
		} else {
			out.WriteString(label + ": ")
		}
		text, err := displayText(ctx, turn.Text, markdown)
		if err != nil {
			return nil, err
		}
		out.WriteString(text)
		out.WriteByte('\n')
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

func timestamp(ms int64) string {
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

func displayText(ctx context.Context, value string, markdown bool) (string, error) {
	var out strings.Builder
	space := false
	for _, r := range value {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if unicode.IsSpace(r) {
			space = out.Len() != 0
			continue
		}
		if space {
			out.WriteByte(' ')
			space = false
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			fmt.Fprintf(&out, "\\u%04x", r)
			continue
		}
		if markdown {
			switch r {
			case '&':
				out.WriteString("&amp;")
				continue
			case '<':
				out.WriteString("&lt;")
				continue
			case '>':
				out.WriteString("&gt;")
				continue
			}
			if strings.ContainsRune("\\`*_{}[]()#+-!|~", r) {
				out.WriteByte('\\')
			}
		}
		out.WriteRune(r)
	}
	return out.String(), nil
}

// subtitles writes one cue per turn on the original recording clock. Turns keep
// their real timing, so a zero-duration turn stays a zero-length cue rather than
// receiving an invented duration.
func subtitles(ctx context.Context, input *Transcript, format Format) ([]byte, error) {
	vtt := format == FormatVTT
	labels := make(map[string]string, len(input.Speakers))
	for _, speaker := range input.Speakers {
		name := fmt.Sprintf("Speaker %d", speaker.Index)
		if speaker.Name != nil {
			name = *speaker.Name
		}
		label, err := displayText(ctx, name, false)
		if err != nil {
			return nil, err
		}
		labels[speaker.ID] = label
	}
	var out strings.Builder
	if vtt {
		out.WriteString("WEBVTT\n")
	}
	for i, turn := range input.Turns {
		label := "Unknown speaker"
		if turn.SpeakerID != nil {
			label = labels[*turn.SpeakerID]
		}
		text, err := displayText(ctx, turn.Text, false)
		if err != nil {
			return nil, err
		}
		if vtt {
			fmt.Fprintf(&out, "\n%s --> %s\n<v %s>%s\n", cueTime(turn.StartMS, '.'), cueTime(turn.EndMS, '.'), vttEscape(label), vttEscape(text))
			continue
		}
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "%d\n%s --> %s\n%s: %s\n", i+1, cueTime(turn.StartMS, ','), cueTime(turn.EndMS, ','), label, text)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

// cueTime formats milliseconds as HH:MM:SS plus the format's decimal separator.
func cueTime(ms int64, separator byte) string {
	return fmt.Sprintf("%02d:%02d:%02d%c%03d", ms/3600000, ms/60000%60, ms/1000%60, separator, ms%1000)
}

var vttEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// vttEscape makes text safe for WebVTT cue text and voice annotations, which
// would otherwise read <, > and & as markup.
func vttEscape(s string) string { return vttEscaper.Replace(s) }
