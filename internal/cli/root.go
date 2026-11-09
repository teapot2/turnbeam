package cli

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"

	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/progress"
	"github.com/teapot2/turnbeam/internal/setup"

	"github.com/spf13/cobra"
)

const (
	groupTranscripts  = "transcripts"
	groupSetup        = "setup"
	groupIntegrations = "integrations"
)

const (
	exitOK       = 0
	exitFailed   = 1
	exitUsage    = 2
	exitCanceled = 130
)

// Run executes one invocation and returns its exit code. Requested output (the
// transcript, help, version, key status, completion scripts) goes to stdout;
// progress and errors go to stderr.
func Run(ctx context.Context, args []string, version string, stdout, stderr io.Writer) int {
	version = resolveVersion(version)
	label := "Error:"
	if progress.Color(stderr) {
		label = ansiBold + ansiRed + "Error:" + ansiReset
	}
	code := exitUsage
	root := newRootWithTranscriber(version, &code, stdout, (pipeline.Runner{}).Run)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := ctx.Err(); err != nil {
		fmt.Fprintln(stderr, label, canceledMessage(ctx, ""))
		return exitCanceled
	}
	if cmd, err := root.ExecuteContextC(ctx); err != nil {
		name := ""
		if cmd != nil {
			name = cmd.Name()
			if model, _ := cmd.Flags().GetString("model"); name == "turnbeam" {
				if _, local := setup.FindLocalModel(model); local {
					name = "turnbeam (local model)"
				}
			}
		}
		var message string
		code, message = reportFailure(ctx, err, code, name)
		fmt.Fprintln(stderr, label, message)
		return code
	}
	return exitOK
}

type transcribeFunc func(context.Context, string, pipeline.Options, io.Writer, io.Writer) error

func newRootWithTranscriber(version string, code *int, stdout io.Writer, run transcribeFunc) *cobra.Command {
	var opts transcribeOptions
	root := &cobra.Command{
		Use:   "turnbeam <recording>",
		Short: "Transcribe a prerecorded meeting",
		Long: "Turnbeam transcribes a meeting recording and labels who spoke when.\n" +
			"Meta Muse transcribes the audio, or a model on your Mac with --model;\n" +
			"speakers are always identified locally on your Mac.",
		Example: "  turnbeam meeting.mp4                      # writes meeting.md and meeting.transcript.json\n" +
			"  turnbeam meeting.mp4 -f srt               # writes meeting.srt\n" +
			"  turnbeam meeting.mp4 -o notes/standup.md\n" +
			"  turnbeam meeting.mp4 -o - -f text | less  # prints to stdout, saves nothing\n" +
			"  turnbeam meeting.mp4 --resume\n" +
			"  turnbeam meeting.mp4 --model parakeet",
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          recordingOrCommand,
		// Complete recording paths as files, alongside subcommand names.
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveDefault
		},
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return opts.validate(cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			*code = exitFailed
			quiet, _ := cmd.Flags().GetBool("quiet")
			verbose, _ := cmd.Flags().GetBool("verbose")
			options := pipeline.Options{Model: opts.model, Mode: opts.mode, Concurrency: opts.concurrency, Resume: opts.resume, Fresh: opts.fresh, InstallDir: opts.installDir}
			options.Render = opts.render()
			options.Output = opts.destination(output.DefaultPath(args[0], options.Render.Format))
			options.Progress.Quiet, options.Progress.Verbose = quiet, verbose
			return run(cmd.Context(), args[0], options, stdout, cmd.ErrOrStderr())
		},
	}
	// Cobra's default edit distance; it applies it only on its own unknown-command path.
	root.SuggestionsMinimumDistance = 2
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		return cmd.Context().Err()
	}
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	opts.addFlags(root)
	root.PersistentFlags().BoolP("quiet", "q", false, "Suppress ordinary progress")
	root.PersistentFlags().Bool("verbose", false, "Write sanitized diagnostics to stderr")
	root.AddGroup(
		&cobra.Group{ID: groupTranscripts, Title: "Work with transcripts:"},
		&cobra.Group{ID: groupSetup, Title: "Setup and diagnostics:"},
		&cobra.Group{ID: groupIntegrations, Title: "Integrations:"},
	)
	addCommands(root, version, code, stdout, run)
	return root
}

// resolveVersion keeps a version set at link time. Otherwise it uses the module
// version Go records for `go install …@version` and VCS builds, so bug reports
// identify the exact build.
func resolveVersion(version string) string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
