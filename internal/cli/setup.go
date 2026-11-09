package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/progress"
	"github.com/teapot2/turnbeam/internal/setup"

	"github.com/spf13/cobra"
)

type setupService interface {
	Install(context.Context) (setup.Installation, error)
	Doctor(context.Context) ([]setup.Check, error)
	LocalStatus(context.Context) ([]setup.ModelStatus, error)
}

// setupFactory builds the service for an installation directory; models names
// local models to install, and onProgress receives Install activity for display.
type setupFactory func(dir string, models []string, onProgress func(setup.Progress)) setupService

func addSetupCommands(root *cobra.Command, code *int, factory setupFactory, keys keyStatus) {
	root.AddCommand(newSetupCmd(code, factory), newDoctorCmd(code, factory, keys), newModelsCmd(code, factory, keys))
}

func newSetupCmd(code *int, factory setupFactory) *cobra.Command {
	var dir string
	var models []string
	cmd := &cobra.Command{
		Use: "setup", Short: "Download and verify the local models", Args: cobra.NoArgs, GroupID: groupSetup,
		Example: "  turnbeam setup\n  turnbeam setup --model parakeet\n  turnbeam setup --install-dir ~/models/turnbeam",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			for _, name := range models {
				if name == pipeline.MetaModel {
					return fmt.Errorf("--model meta needs no download; local models: %s", strings.Join(modelNames()[1:], ", "))
				}
				if err := choice("model", name, modelNames()[1:]...); err != nil {
					return err
				}
			}
			return validateInstallDir(cmd, dir)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			quiet, reporter := setupReporter(cmd)
			what, done := "the speaker runtime and model", "Installed and verified. Run `turnbeam doctor` to check all dependencies."
			var extra []string
			for _, name := range models {
				model, _ := setup.FindLocalModel(name)
				extra = append(extra, fmt.Sprintf("%s (%s)", model.Title, megabytes(model.Size())))
			}
			if len(extra) > 0 {
				what += ", plus " + strings.Join(extra, " and ")
			}
			if len(models) > 0 {
				done = fmt.Sprintf("Installed and verified. Transcribe on your Mac with `turnbeam <recording> --model %s`.", models[0])
			}
			if !quiet {
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Installing %s into %s\nEvery file is checked against a SHA-256 hash pinned in Turnbeam; license notices are kept.\n", what, displayDir(dir)); err != nil {
					return err
				}
			}
			_, err := factory(dir, models, func(p setup.Progress) { reporter.Setup(p) }).Install(cmd.Context())
			// End any open progress bar before the result or error is printed.
			reporter.Close()
			if err != nil || quiet {
				return err
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), done)
			return err
		},
	}
	addInstallDirFlag(cmd, &dir)
	cmd.Flags().StringSliceVar(&models, "model", nil, "Also install the local transcription model with this `name`: "+strings.Join(modelNames()[1:], ", ")+" (repeatable; see turnbeam models)")
	completeChoices(cmd, "model", modelNames()[1:])
	return cmd
}

func newDoctorCmd(code *int, factory setupFactory, keys keyStatus) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use: "doctor", Short: "Check that Turnbeam is ready to transcribe (offline)", Args: cobra.NoArgs, GroupID: groupSetup,
		PreRunE: func(cmd *cobra.Command, _ []string) error { return validateInstallDir(cmd, dir) },
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			quiet, _ := setupReporter(cmd)
			return runDoctor(cmd, factory(dir, nil, nil), keys, dir, quiet)
		},
	}
	addInstallDirFlag(cmd, &dir)
	return cmd
}

func setupReporter(cmd *cobra.Command) (bool, *progress.Reporter) {
	quiet, _ := cmd.Flags().GetBool("quiet")
	verbose, _ := cmd.Flags().GetBool("verbose")
	return quiet, progress.New(cmd.ErrOrStderr(), progress.Options{Quiet: quiet, Verbose: verbose})
}

// displayDir names the installation directory for people, abbreviating $HOME.
func displayDir(dir string) string {
	if dir == "" {
		var err error
		if dir, err = setup.DefaultDir(); err != nil {
			return "the managed installation directory"
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rest, ok := strings.CutPrefix(dir, home+string(filepath.Separator)); ok {
			return filepath.Join("~", rest)
		}
	}
	return dir
}
