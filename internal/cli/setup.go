package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/teapot2/turnbeam/internal/apple"
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
	var dir, language string
	var models []string
	cmd := &cobra.Command{
		Use: "setup", Short: "Download and verify the local models", Args: cobra.NoArgs, GroupID: groupSetup,
		Example: "  turnbeam setup\n  turnbeam setup --model parakeet\n  turnbeam setup --model apple --language es-MX\n  turnbeam setup --install-dir ~/models/turnbeam",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			for _, name := range models {
				if name == pipeline.MetaModel {
					return fmt.Errorf("--model meta needs no download; local models: %s", strings.Join(modelNames()[1:], ", "))
				}
				if err := choice("model", name, modelNames()[1:]...); err != nil {
					return err
				}
			}
			if cmd.Flags().Changed("language") {
				if !slices.Contains(models, apple.Model) {
					return errors.New("--language applies only to --model apple")
				}
				if !apple.ValidLanguage(language) {
					return fmt.Errorf("--language must be a language tag such as en-US or es-MX")
				}
			}
			return validateInstallDir(cmd, dir)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			quiet, reporter := setupReporter(cmd)
			what, done := "the speaker runtime and model", "Installed and verified. Run `turnbeam doctor` to check all dependencies."
			// Apple's transcriber is not a pinned download: it is set up after.
			withApple := slices.Contains(models, apple.Model)
			pinned := slices.DeleteFunc(slices.Clone(models), func(name string) bool { return name == apple.Model })
			var extra []string
			for _, name := range pinned {
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
			_, err := factory(dir, pinned, func(p setup.Progress) { reporter.Setup(p) }).Install(cmd.Context())
			// End any open progress bar before the result or error is printed.
			reporter.Close()
			if err != nil {
				return err
			}
			if withApple {
				if err := setupApple(cmd, dir, language, quiet); err != nil {
					return err
				}
			}
			if quiet {
				return nil
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), done)
			return err
		},
	}
	addInstallDirFlag(cmd, &dir)
	cmd.Flags().StringVar(&language, "language", "", "Language to set up for --model apple, such as en-US or es-MX (default: your Mac's language)")
	cmd.Flags().StringSliceVar(&models, "model", nil, "Also install the local transcription model with this `name`: "+strings.Join(modelNames()[1:], ", ")+" (repeatable; see turnbeam models)")
	completeChoices(cmd, "model", modelNames()[1:])
	return cmd
}

// setupApple builds the Apple Speech helper and asks macOS for Apple's speech
// model in one language. macOS downloads and keeps that model itself.
func setupApple(cmd *cobra.Command, dir, language string, quiet bool) error {
	say := func(format string, a ...any) error {
		if quiet {
			return nil
		}
		_, err := fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...)
		return err
	}
	helper := appleHelper(dir)
	if err := say("==> Building the Apple Speech helper (Xcode Command Line Tools)"); err != nil {
		return err
	}
	if err := helper.Build(cmd.Context()); err != nil {
		return err
	}
	if err := say("==> Getting Apple's speech model from macOS"); err != nil {
		return err
	}
	state, err := helper.Install(cmd.Context(), language)
	if err != nil {
		return err
	}
	if state.Status != "installed" {
		return fmt.Errorf("Apple's speech model for %s is %s, not installed yet; run setup again in a few minutes", state.Locale, state.Status)
	}
	return say("    %s ready", state.Locale)
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
