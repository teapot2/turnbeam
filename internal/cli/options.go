package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/teapot2/turnbeam/internal/apple"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/settings"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/transcript"

	"github.com/spf13/cobra"
)

type outputOptions struct {
	format     string
	output     string
	timestamps string
}

// stdoutPath is the --output value that writes the transcript to stdout.
const stdoutPath = "-"

// addFlags registers the output flags; fallback describes where output goes
// without --output.
func (o *outputOptions) addFlags(cmd *cobra.Command, fallback string) {
	cmd.Flags().StringVarP(&o.format, "format", "f", string(transcript.FormatMarkdown), "Output format: "+joinChoices(transcript.Formats))
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "Output file, or - for stdout (default: "+fallback+")")
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

// destination returns the path to write, "" for stdout, or fallback when
// --output is not set.
func (o outputOptions) destination(fallback string) string {
	switch o.output {
	case stdoutPath:
		return ""
	case "":
		return fallback
	}
	return o.output
}

func (o outputOptions) render() transcript.RenderOptions {
	return transcript.RenderOptions{Format: transcript.Format(o.format), Timestamps: transcript.Timestamps(o.timestamps)}
}

// validate checks the output flags. Without an explicit --format, a known
// output extension (.srt, .vtt, .json, .txt, .md) selects the format.
func (o *outputOptions) validate(cmd *cobra.Command) error {
	if !cmd.Flags().Changed("format") {
		if format, ok := transcript.FormatForExtension(o.output); ok {
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
	language    string
}

func (o *transcribeOptions) addFlags(cmd *cobra.Command) {
	o.outputOptions.addFlags(cmd, "next to the recording, with the format's extension")
	cmd.Flags().StringVar(&o.model, "model", "", "Transcription model `name`: meta (Meta Muse), "+strings.Join(modelNames()[1:], ", ")+" on your Mac (see turnbeam models), or a NeMo-Speech.cpp .gguf file of your own (default: "+modelEnv+", else the model saved with `turnbeam models default`, else the first installed local model, else meta)")
	cmd.Flags().StringVar(&o.language, "language", "", "Language of the recording for --model apple, such as en-US or es-MX (default: your Mac's language)")
	cmd.RegisterFlagCompletionFunc("language", cobra.NoFileCompletions)
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
	if err := validateInstallDir(cmd, o.installDir); err != nil {
		return err
	}
	source := "--model"
	if !cmd.Flags().Changed("model") {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		name, from, err := defaultModel(ctx, o.installDir)
		if err != nil {
			return err
		}
		o.model, source = name, from
	}
	if err := choice("model", o.model, modelNames()...); err != nil && !setup.IsModelFile(o.model) {
		switch source {
		case "--model":
			return err
		case fromEnv:
			return fmt.Errorf("%s must be one of: %s", modelEnv, joinChoices(modelNames()))
		}
		return fmt.Errorf("the default model %q (%s) is not available; choose one with `turnbeam models default NAME`", o.model, source)
	}
	if cmd.Flags().Changed("language") {
		if o.model != apple.Model {
			message := "--language applies only to --model apple"
			if source != "--model" {
				message += fmt.Sprintf(" (the default model is %s, %s)", o.model, source)
			}
			return errors.New(message)
		}
		if !apple.ValidLanguage(o.language) {
			return fmt.Errorf("--language must be a language tag such as en-US or es-MX")
		}
	}
	if _, local := setup.FindLocalModel(o.model); local || setup.IsModelFile(o.model) || o.model == apple.Model {
		for _, flag := range []string{"mode", "concurrency", "resume", "fresh"} {
			if !cmd.Flags().Changed(flag) {
				continue
			}
			message := fmt.Sprintf("--%s applies only to --model meta; local models use no Meta requests or checkpoints", flag)
			if source != "--model" {
				message += fmt.Sprintf(" (the default model is %s, %s)", o.model, source)
			}
			return errors.New(message)
		}
	}
	return nil
}

// modelEnv names the environment variable that sets the default --model.
const modelEnv = "TURNBEAM_MODEL"

// Where a default model came from, for messages.
const (
	fromEnv       = modelEnv
	fromSaved     = "your saved default"
	fromInstalled = "the first installed local model"
	fromBuiltIn   = "the built-in default"
)

// defaultModel picks the model a run uses when --model is not given, and says
// why: TURNBEAM_MODEL, then the saved default, then the first installed local
// model, then meta. An unreadable settings file or installation is an error,
// never a silent switch to the Meta API.
func defaultModel(ctx context.Context, installDir string) (name, source string, err error) {
	if model := strings.TrimSpace(os.Getenv(modelEnv)); model != "" {
		return model, fromEnv, nil
	}
	path, err := settings.Path()
	if err != nil {
		return "", "", err
	}
	saved, err := settings.Load(path)
	if err != nil {
		return "", "", err
	}
	if saved.DefaultModel != "" {
		return saved.DefaultModel, fromSaved, nil
	}
	installed, err := (setup.Manager{Dir: installDir}).InstalledModels(ctx)
	if err != nil {
		return "", "", fmt.Errorf("find installed models for the default: %w", err)
	}
	if len(installed) > 0 {
		return installed[0], fromInstalled, nil
	}
	if appleHelper(installDir).Built() {
		return apple.Model, fromInstalled, nil
	}
	return pipeline.MetaModel, fromBuiltIn, nil
}

// appleService builds and queries the Apple Speech helper; tests replace it.
type appleService interface {
	Built() bool
	Build(context.Context) error
	Status(context.Context, string) (apple.State, error)
	Install(context.Context, string) (apple.State, error)
}

var appleHelper = func(dir string) appleService { return apple.Helper{Dir: dir} }

// modelNames lists every --model value: Meta first, then the pinned local
// models, then Apple's on-device transcriber.
func modelNames() []string {
	names := []string{pipeline.MetaModel}
	for _, model := range setup.LocalModels() {
		names = append(names, model.Name)
	}
	names = append(names, apple.Model)
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
