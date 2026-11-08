package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/teapot2/turnbeam/internal/progress"
	"github.com/teapot2/turnbeam/internal/setup"

	"github.com/spf13/cobra"
)

type setupService interface {
	Install(context.Context) (setup.Installation, error)
	Doctor(context.Context) ([]setup.Check, error)
}

// setupFactory builds the service for an installation directory; onProgress
// receives Install activity for display.
type setupFactory func(dir string, onProgress func(setup.Progress)) setupService

func addSetupCommands(root *cobra.Command, code *int, factory setupFactory, keys keyStatus) {
	root.AddCommand(newSetupCmd(code, factory), newDoctorCmd(code, factory, keys))
}

func newSetupCmd(code *int, factory setupFactory) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use: "setup", Short: "Download and verify the local speaker model", Args: cobra.NoArgs, GroupID: groupSetup,
		Example: "  turnbeam setup\n  turnbeam setup --install-dir ~/models/turnbeam",
		PreRunE: func(cmd *cobra.Command, _ []string) error { return validateInstallDir(cmd, dir) },
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			quiet, reporter := setupReporter(cmd)
			if !quiet {
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Installing the speaker runtime and model into %s\nEvery file is checked against a SHA-256 hash pinned in Turnbeam; license notices are kept.\n", displayDir(dir)); err != nil {
					return err
				}
			}
			_, err := factory(dir, func(p setup.Progress) { reporter.Setup(p) }).Install(cmd.Context())
			// End any open progress bar before the result or error is printed.
			reporter.Close()
			if err != nil || quiet {
				return err
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), "Installed and verified. Run `turnbeam doctor` to check all dependencies.")
			return err
		},
	}
	addInstallDirFlag(cmd, &dir)
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
			return runDoctor(cmd, factory(dir, nil), keys, dir, quiet)
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
