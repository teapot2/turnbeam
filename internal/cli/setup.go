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
	for _, name := range []string{"setup", "doctor"} {
		var dir string
		cmd := &cobra.Command{
			Use: name, Args: cobra.NoArgs,
			PreRunE: func(cmd *cobra.Command, _ []string) error {
				return validateInstallDir(cmd, dir)
			},
			RunE: func(cmd *cobra.Command, _ []string) error {
				*code = exitFailed
				quiet, _ := cmd.Flags().GetBool("quiet")
				verbose, _ := cmd.Flags().GetBool("verbose")
				reporter := progress.New(cmd.ErrOrStderr(), progress.Options{Quiet: quiet, Verbose: verbose})
				service := factory(dir, func(p setup.Progress) { reporter.Setup(p) })
				if name == "setup" {
					if !quiet {
						if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Installing the speaker runtime and model into %s\nEvery file is checked against a SHA-256 hash pinned in Turnbeam; license notices are kept.\n", displayDir(dir)); err != nil {
							return err
						}
					}
					_, err := service.Install(cmd.Context())
					// End any open progress bar before the result or error is printed.
					reporter.Close()
					if err != nil {
						return err
					}
					if !quiet {
						_, err := fmt.Fprintln(cmd.ErrOrStderr(), "Installed and verified. Run `turnbeam doctor` to check all dependencies.")
						return err
					}
					return nil
				}
				return runDoctor(cmd, service, keys, dir, quiet)
			},
		}
		if name == "setup" {
			cmd.Short = "Download and verify the local speaker model"
			cmd.Example = "  turnbeam setup\n  turnbeam setup --install-dir ~/models/turnbeam"
		} else {
			cmd.Short = "Check that Turnbeam is ready to transcribe (offline)"
		}
		addInstallDirFlag(cmd, &dir)
		cmd.GroupID = groupSetup
		root.AddCommand(cmd)
	}
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
