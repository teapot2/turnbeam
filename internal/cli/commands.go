package cli

import (
	"fmt"
	"io"

	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/transcript"

	"github.com/spf13/cobra"
)

func addCommands(root *cobra.Command, version string, code *int, stdout io.Writer, run transcribeFunc) {
	factory := func(dir string, models []string, onProgress func(setup.Progress)) setupService {
		return setup.Manager{Dir: dir, Models: models, OnProgress: onProgress}
	}
	keys := credentials.Provider{}.Status
	addSetupCommands(root, code, factory, keys)
	root.AddCommand(newMCPCmd(version, code, stdout, run, factory, keys))
	root.AddCommand(&cobra.Command{
		Use: "version", Short: "Print the application version", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "turnbeam", version)
			return err
		},
	})

	showHelp := func(cmd *cobra.Command, _ []string) error {
		*code = exitFailed
		return cmd.Help()
	}
	addKeyCommands(root, code, defaultKeys)

	speakers := &cobra.Command{Use: "speakers", Short: "Name the speakers in a transcript", Args: cobra.NoArgs, RunE: showHelp, GroupID: groupTranscripts}
	var names []string
	var mappings map[int]string
	rename := &cobra.Command{
		Use: "rename <transcript.json>", Short: "Set speaker display names in a JSON sidecar", Args: onePath,
		Example:           "  turnbeam speakers rename meeting.transcript.json --speaker 1=Alice --speaker 2=Bob",
		ValidArgsFunction: completeJSON,
		PreRunE: func(_ *cobra.Command, _ []string) (err error) {
			mappings, err = transcript.ParseSpeakerNames(names)
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			*code = exitFailed
			if err := output.Rename(cmd.Context(), args[0], mappings); err != nil {
				return err
			}
			if quiet, _ := cmd.Flags().GetBool("quiet"); quiet {
				return nil
			}
			noun := "names"
			if len(mappings) == 1 {
				noun = "name"
			}
			_, err := fmt.Fprintf(cmd.ErrOrStderr(), "Updated %d speaker %s in the JSON sidecar. Run `turnbeam export` to write a transcript with them.\n", len(mappings), noun)
			return err
		},
	}
	rename.Flags().StringArrayVar(&names, "speaker", nil, "Speaker display name: INDEX=NAME (repeatable)")
	rename.RegisterFlagCompletionFunc("speaker", cobra.NoFileCompletions)
	speakers.AddCommand(rename)
	root.AddCommand(speakers)

	var opts outputOptions
	export := &cobra.Command{
		Use: "export <transcript.json>", Short: "Write a transcript from its JSON sidecar", Args: onePath,
		GroupID:           groupTranscripts,
		Example:           "  turnbeam export meeting.transcript.json -o meeting-named.md\n  turnbeam export meeting.transcript.json -f text > meeting.txt",
		ValidArgsFunction: completeJSON,
		PreRunE:           func(cmd *cobra.Command, _ []string) error { return opts.validate(cmd) },
		RunE: func(cmd *cobra.Command, args []string) error {
			*code = exitFailed
			return output.Export(cmd.Context(), args[0], opts.destination(""), opts.render(), stdout)
		},
	}
	opts.addFlags(export, "stdout")
	root.AddCommand(export)
}

// completeJSON completes transcript sidecars only.
func completeJSON(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return []cobra.Completion{"json"}, cobra.ShellCompDirectiveFilterFileExt
}
