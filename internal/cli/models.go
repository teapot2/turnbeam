package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/teapot2/turnbeam/internal/apple"
	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/settings"
	"github.com/teapot2/turnbeam/internal/setup"

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
			def, _, err := defaultModel(cmd.Context(), dir)
			if err != nil {
				return err
			}
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
			fmt.Fprintf(w, "%s\ton device\t-\tmany; choose with --language\tApple (macOS)\t%s\n", name(apple.Model), appleStatus(cmd.Context(), dir))
			if err := w.Flush(); err != nil {
				return err
			}
			if quiet, _ := cmd.Flags().GetBool("quiet"); quiet {
				return nil
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), "Choose with --model NAME, or save a default with `turnbeam models default NAME`. --model FILE.gguf runs a NeMo-Speech.cpp model file of your own.")
			return err
		},
	}
	addInstallDirFlag(cmd, &dir)
	cmd.AddCommand(newModelsDefaultCmd(code))
	return cmd
}

// automatic is the `models default` argument that removes a saved default.
const automatic = "auto"

func newModelsDefaultCmd(code *int) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "default [NAME|auto]",
		Short: "Show or save the model used when --model is not given",
		Long: "Without a name, prints the model a transcription uses when --model is not given,\n" +
			"and why. With a name, saves it as your default; `auto` removes the saved default.\n\n" +
			"The default is " + modelEnv + " if set, else the saved model, else the first\n" +
			"installed local model, else meta.",
		Example: "  turnbeam models default\n  turnbeam models default nemotron-en\n  turnbeam models default auto",
		Args:    cobra.MaximumNArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return append(modelNames(), automatic), cobra.ShellCompDirectiveNoFileComp
		},
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if err := validateInstallDir(cmd, dir); err != nil {
				return err
			}
			if len(args) == 0 || args[0] == automatic || setup.IsModelFile(args[0]) {
				return nil
			}
			if err := choice("model", args[0], modelNames()...); err != nil {
				return fmt.Errorf("model must be one of: %s, %s, or a .gguf file", joinChoices(modelNames()), automatic)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			*code = exitFailed
			quiet, _ := cmd.Flags().GetBool("quiet")
			note := func(format string, a ...any) error {
				if quiet {
					return nil
				}
				_, err := fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...)
				return err
			}
			if len(args) == 0 {
				name, source, err := defaultModel(cmd.Context(), dir)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), name); err != nil {
					return err
				}
				return note("From %s. Save another with `turnbeam models default NAME`.", source)
			}
			path, err := settings.Path()
			if err != nil {
				return err
			}
			saved, err := settings.Load(path)
			if err != nil {
				return err
			}
			name := args[0]
			if setup.IsModelFile(name) {
				if name, err = modelFile(name); err != nil {
					return err
				}
			}
			if name == automatic {
				name = ""
			}
			saved.DefaultModel = name
			if err := settings.Save(path, saved); err != nil {
				return err
			}
			if name == "" {
				current, source, err := defaultModel(cmd.Context(), dir)
				if err != nil {
					return err
				}
				return note("Removed the saved default. Transcriptions now use %s (%s).", current, source)
			}
			if err := note("Saved %s as the default model.", name); err != nil {
				return err
			}
			if _, local := setup.FindLocalModel(name); local {
				installed, err := (setup.Manager{Dir: dir}).InstalledModels(cmd.Context())
				if err == nil && !slices.Contains(installed, name) {
					if err := note("It is not installed yet; run `turnbeam setup --model %s`.", name); err != nil {
						return err
					}
				}
			}
			if env := strings.TrimSpace(os.Getenv(modelEnv)); env != "" {
				return note("%s=%s is set and still takes precedence.", modelEnv, env)
			}
			return nil
		},
	}
	addInstallDirFlag(cmd, &dir)
	return cmd
}

// modelFile checks a .gguf default without reading the whole file and returns
// its absolute path, so the default works from any directory.
func modelFile(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errors.Join(setup.ErrCustomModel, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", errors.Join(setup.ErrCustomModel, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", setup.ErrCustomModel
	}
	return abs, nil
}

// appleStatus reports Apple's transcriber for the Mac's language. macOS keeps
// the speech model, so there is no download size to show.
func appleStatus(ctx context.Context, dir string) string {
	helper := appleHelper(dir)
	if !helper.Built() {
		return "not set up; run `turnbeam setup --model apple`"
	}
	state, err := helper.Status(ctx, "")
	switch {
	case errors.Is(err, apple.ErrUnavailable):
		return "needs macOS 26 or later"
	case err != nil:
		return "could not be checked"
	case state.Status == "installed":
		return "installed (" + state.Locale + ")"
	}
	return state.Locale + " not downloaded; run `turnbeam setup --model apple`"
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
