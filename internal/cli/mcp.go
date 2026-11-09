package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/teapot2/turnbeam/internal/mcp"

	"github.com/spf13/cobra"
)

func newMCPCmd(version string, code *int, stdout io.Writer, run transcribeFunc, factory setupFactory, keys keyStatus) *cobra.Command {
	var roots []string
	var dir string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve Turnbeam's tools to an AI assistant over stdio (Model Context Protocol)",
		Long: "turnbeam mcp speaks the Model Context Protocol on stdin and stdout, so an\n" +
			"assistant such as Claude Desktop can find recordings, transcribe them, and read,\n" +
			"rename and export transcripts on this Mac. Nothing is hosted anywhere: the\n" +
			"assistant you connect receives whatever transcript text it asks for.\n\n" +
			"Tools read and write only under --root (default: your home folder).",
		Example: "  turnbeam mcp\n  turnbeam mcp --root ~/Meetings --root ~/Downloads",
		Args:    cobra.NoArgs, GroupID: groupIntegrations,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateInstallDir(cmd, dir); err != nil {
				return err
			}
			// The server runs only catalog models; a saved default is checked per call.
			if env := strings.TrimSpace(os.Getenv(modelEnv)); env != "" && choice("model", env, modelNames()...) != nil {
				return fmt.Errorf("%s must be one of: %s", modelEnv, joinChoices(modelNames()))
			}
			return mcp.CheckRoots(roots)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			verbose, _ := cmd.Flags().GetBool("verbose")
			server := &mcp.Server{
				Version: version, Roots: roots, InstallDir: dir,
				Default: func(ctx context.Context) (string, error) {
					name, _, err := defaultModel(ctx, dir)
					return name, err
				},
				Transcribe: run, Installation: factory(dir, nil, nil), KeyStatus: keys,
				Describe: describeFailure, Stderr: cmd.ErrOrStderr(), Verbose: verbose,
			}
			return server.Serve(cmd.Context(), cmd.InOrStdin(), stdout)
		},
	}
	cmd.Flags().StringArrayVar(&roots, "root", nil, "Directory the assistant may read and write (repeatable; default: your home folder)")
	cmd.RegisterFlagCompletionFunc("root", func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveFilterDirs
	})
	addInstallDirFlag(cmd, &dir)
	return cmd
}

// describeFailure writes an error for an assistant the way the CLI writes it
// for a terminal: classified, without paths, responses or keys.
func describeFailure(err error) string {
	class, message := classify(err)
	return string(class) + ": " + message
}
