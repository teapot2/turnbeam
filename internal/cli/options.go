package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/transcript"

	"github.com/spf13/cobra"
)

type outputOptions struct {
	format     string
	output     string
	timestamps string
}

func (o *outputOptions) addFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&o.format, "format", "f", string(transcript.FormatMarkdown), "Output format: "+joinChoices(transcript.Formats))
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "Output file (default: stdout)")
	cmd.Flags().StringVar(&o.timestamps, "timestamps", string(transcript.TimestampsStart), "Timestamp display: "+joinChoices(transcript.TimestampStyles))
	completeChoices(cmd, "format", transcript.Formats)
	completeChoices(cmd, "timestamps", transcript.TimestampStyles)
}

// completeChoices offers a flag's fixed values to shell completion.
func completeChoices[T ~string](cmd *cobra.Command, flag string, values []T) {
	names := make([]cobra.Completion, len(values))
	for i, value := range values {
		names[i] = string(value)
	}
	cmd.RegisterFlagCompletionFunc(flag, cobra.FixedCompletions(names, cobra.ShellCompDirectiveNoFileComp))
}

func (o outputOptions) render() transcript.RenderOptions {
	return transcript.RenderOptions{Format: transcript.Format(o.format), Timestamps: transcript.Timestamps(o.timestamps)}
}

// validate checks the output flags. Without an explicit --format, a known
// output extension (.srt, .vtt, .json, .txt, .md) selects the format.
func (o *outputOptions) validate(cmd *cobra.Command) error {
	if !cmd.Flags().Changed("format") {
		if format, ok := formatForExtension(o.output); ok {
			o.format = string(format)
		}
	}
	if err := choice("format", transcript.Format(o.format), transcript.Formats...); err != nil {
		return err
	}
	if err := choice("timestamps", transcript.Timestamps(o.timestamps), transcript.TimestampStyles...); err != nil {
		return err
	}
	if cmd.Flags().Changed("output") && strings.TrimSpace(o.output) == "" {
		return fmt.Errorf("--output must not be empty")
	}
	return nil
}

type transcribeOptions struct {
	outputOptions
	model       string
	mode        string
	concurrency int
	resume      bool
	fresh       bool
	installDir  string
}

func (o *transcribeOptions) addFlags(cmd *cobra.Command) {
	o.outputOptions.addFlags(cmd)
	cmd.Flags().StringVar(&o.model, "model", defaultModel(), "Transcription model `name`: meta (Meta Muse), or "+strings.Join(modelNames()[1:], " or ")+" on your Mac (see turnbeam models); "+modelEnv+" sets the default")
	cmd.Flags().StringVar(&o.mode, "mode", "diarization", "Meta mode: diarization (best speaker labels), endpointing")
	cmd.Flags().IntVar(&o.concurrency, "concurrency", meta.DefaultConcurrency, "Maximum concurrent Meta requests")
	cmd.Flags().BoolVar(&o.resume, "resume", false, "Continue an interrupted run without repeating paid work")
	cmd.Flags().BoolVar(&o.fresh, "fresh", false, "Start over instead of resuming (repeats paid work)")
	completeChoices(cmd, "model", modelNames())
	completeChoices(cmd, "mode", []string{"diarization", "endpointing"})
	cmd.RegisterFlagCompletionFunc("concurrency", cobra.NoFileCompletions)
	addInstallDirFlag(cmd, &o.installDir)
}

func (o *transcribeOptions) validate(cmd *cobra.Command) error {
	if err := o.outputOptions.validate(cmd); err != nil {
		return err
	}
	if err := choice("mode", o.mode, "diarization", "endpointing"); err != nil {
		return err
	}
	if o.concurrency < 1 {
		return fmt.Errorf("--concurrency must be greater than zero")
	}
	if o.resume && o.fresh {
		return fmt.Errorf("--resume and --fresh are mutually exclusive")
	}
	if err := choice("model", o.model, modelNames()...); err != nil {
		if !cmd.Flags().Changed("model") {
			return fmt.Errorf("%s must be one of: %s", modelEnv, joinChoices(modelNames()))
		}
		return err
	}
	if _, local := setup.FindLocalModel(o.model); local {
		for _, flag := range []string{"mode", "concurrency", "resume", "fresh"} {
			if cmd.Flags().Changed(flag) {
				return fmt.Errorf("--%s applies only to --model meta; local models use no Meta requests or checkpoints", flag)
			}
		}
	}
	return validateInstallDir(cmd, o.installDir)
}

// modelEnv names the environment variable that sets the default --model.
const modelEnv = "TURNBEAM_MODEL"

// defaultModel is TURNBEAM_MODEL when set, otherwise Meta Muse.
func defaultModel() string {
	if model := strings.TrimSpace(os.Getenv(modelEnv)); model != "" {
		return model
	}
	return pipeline.MetaModel
}

// modelNames lists every --model value: Meta first, then the local models.
func modelNames() []string {
	names := []string{pipeline.MetaModel}
	for _, model := range setup.LocalModels() {
		names = append(names, model.Name)
	}
	return names
}

func addInstallDirFlag(cmd *cobra.Command, dir *string) {
	cmd.Flags().StringVar(dir, "install-dir", "", "Runtime and model directory (default: Application Support)")
	cmd.RegisterFlagCompletionFunc("install-dir", func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveFilterDirs
	})
}

func validateInstallDir(cmd *cobra.Command, dir string) error {
	if cmd.Flags().Changed("install-dir") && strings.TrimSpace(dir) == "" {
		return fmt.Errorf("--install-dir must not be empty")
	}
	return nil
}

func choice[T ~string](flag string, value T, allowed ...T) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("--%s must be one of: %s", flag, joinChoices(allowed))
}

func joinChoices[T ~string](values []T) string {
	names := make([]string, len(values))
	for i, value := range values {
		names[i] = string(value)
	}
	return strings.Join(names, ", ")
}

// recordingOrCommand accepts one recording path. When the argument is not an
// existing file but is close to a subcommand name, it reports a usage error with
// Cobra's own suggestions instead of a confusing missing-recording error.
func recordingOrCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		// Bare `turnbeam` shows help; flags without a recording are a mistake.
		if cmd.Flags().NFlag() > 0 {
			return errors.New("missing <recording>; run `turnbeam --help` for usage")
		}
		return nil
	}
	if err := onePath(cmd, args); err != nil {
		return err
	}
	if _, err := os.Stat(args[0]); err == nil {
		return nil
	}
	if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
		return fmt.Errorf("unknown command %q; did you mean %q? Run `turnbeam --help` for commands", args[0], suggestions[0])
	}
	return nil
}

func onePath(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(1)(cmd, args); err != nil {
		return err
	}
	if strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("input path must not be empty")
	}
	return nil
}

func formatForExtension(path string) (transcript.Format, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".srt":
		return transcript.FormatSRT, true
	case ".vtt":
		return transcript.FormatVTT, true
	case ".json":
		return transcript.FormatJSON, true
	case ".txt":
		return transcript.FormatText, true
	case ".md", ".markdown":
		return transcript.FormatMarkdown, true
	}
	return "", false
}
