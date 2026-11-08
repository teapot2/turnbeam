package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/teapot2/turnbeam/internal/credentials"

	"github.com/spf13/cobra"
)

type keyService interface {
	Status(context.Context) (credentials.Source, bool, error)
	Save(context.Context, []byte) error
	Remove(context.Context) error
}

type keyDependencies struct {
	service   keyService
	read      func(context.Context, io.Writer) ([]byte, error)
	supported bool
}

func defaultKeys() keyDependencies {
	return keyDependencies{credentials.Provider{}, credentials.ReadSecret, credentials.KeychainSupported}
}

// addKeyCommands manages the Meta API key. Turnbeam has no account or session,
// so the commands are named for what they do to the stored key.
func addKeyCommands(root *cobra.Command, code *int, deps func() keyDependencies) {
	noArgs := func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return errors.New("unknown command or extra arguments for key; use `turnbeam key set` to enter a key securely")
		}
		return nil
	}
	key := &cobra.Command{
		Use: "key", Short: "Manage the Meta API key", Args: noArgs, GroupID: groupSetup,
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			return cmd.Help()
		},
	}
	// Parser diagnostics must not echo a mistakenly supplied key or flag value.
	key.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error {
		return errors.New("invalid key options; use `turnbeam key --help`")
	})
	key.AddCommand(&cobra.Command{
		Use: "set", Short: "Save a Meta API key in the macOS Keychain", Args: noArgs,
		Example: "  turnbeam key set\n" +
			"  export META_API_KEY=...   # or use an environment variable instead",
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			d := deps()
			if !d.supported {
				return credentials.ErrUnavailable
			}
			secret, err := d.read(cmd.Context(), cmd.ErrOrStderr())
			defer clear(secret)
			if err != nil {
				return err
			}
			if err := d.service.Save(cmd.Context(), secret); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), "Saved your Meta API key in the macOS Keychain. META_API_KEY, if set, takes precedence.")
			return err
		},
	})
	key.AddCommand(&cobra.Command{
		Use: "status", Short: "Show whether a Meta API key is available, and from where", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			source, available, err := deps().service.Status(cmd.Context())
			if err != nil {
				return err
			}
			message := "No Meta API key found. Run `turnbeam key set`, or set META_API_KEY."
			switch {
			case available && source == credentials.Environment:
				message = "Using the Meta API key from the META_API_KEY environment variable."
			case available:
				message = "Using the Meta API key saved in the macOS Keychain."
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), message)
			return err
		},
	})
	key.AddCommand(&cobra.Command{
		Use: "remove", Short: "Remove the saved Meta API key from the macOS Keychain", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = exitFailed
			if err := deps().service.Remove(cmd.Context()); err != nil {
				return err
			}
			_, err := fmt.Fprintln(cmd.ErrOrStderr(), "Removed the Meta API key from the macOS Keychain. META_API_KEY in your environment is unaffected.")
			return err
		},
	})
	root.AddCommand(key)
}
