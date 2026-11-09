package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/pipeline"

	"github.com/spf13/cobra"
)

func newModelsCmd(code *int, factory setupFactory, keys keyStatus) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use: "models", Short: "List transcription models and whether they are ready (offline)", Args: cobra.NoArgs, GroupID: groupSetup,
		PreRunE: func(cmd *cobra.Command, _ []string) error { return validateInstallDir(cmd, dir) },
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			statuses, err := factory(dir, nil, nil).LocalStatus(cmd.Context())
			if err != nil {
				return err
			}
			def := defaultModel()
			name := func(model string) string {
				if model == def {
					return model + " (default)"
				}
				return model
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "MODEL\tRUNS\tSIZE\tLANGUAGES\tLICENSE\tSTATUS")
			fmt.Fprintf(w, "%s\tMeta API\t-\t-\tMeta API terms\t%s\n", name(pipeline.MetaModel), metaStatus(cmd, keys))
			for _, s := range statuses {
				status := "installed"
				switch {
				case s.Err != nil:
					status = "damaged; run `turnbeam setup --model " + s.Model.Name + "`"
				case !s.Installed:
					status = "not installed; run `turnbeam setup --model " + s.Model.Name + "`"
				}
				fmt.Fprintf(w, "%s\ton device\t%s\t%s\t%s\t%s\n", name(s.Model.Name), megabytes(s.Model.Size()), s.Model.Languages, s.Model.License, status)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if quiet, _ := cmd.Flags().GetBool("quiet"); quiet {
				return nil
			}
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "Choose with --model NAME, or set %s to change the default.\n", modelEnv)
			return err
		},
	}
	addInstallDirFlag(cmd, &dir)
	return cmd
}

// metaStatus reports whether Meta Muse can be used, without loading the key.
func metaStatus(cmd *cobra.Command, keys keyStatus) string {
	source, available, err := keys(cmd.Context())
	switch {
	case err != nil:
		return "key could not be read"
	case !available:
		return "needs a key; run `turnbeam key set`"
	case source == credentials.Environment:
		return "ready (META_API_KEY)"
	}
	return "ready (key in Keychain)"
}

// megabytes formats a download size in decimal megabytes, like setup does.
func megabytes(size int64) string {
	return fmt.Sprintf("%d MB", (size+500_000)/1_000_000)
}
