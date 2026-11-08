package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/progress"
	"github.com/teapot2/turnbeam/internal/setup"

	"github.com/spf13/cobra"
)

// keyStatus reports whether a Meta API key is available, without loading it.
type keyStatus func(context.Context) (credentials.Source, bool, error)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiGreen  = "\x1b[32m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
)

func runDoctor(cmd *cobra.Command, service setupService, keys keyStatus, dir string, quiet bool) error {
	w := cmd.ErrOrStderr()
	color := progress.Color(w)
	if !quiet {
		if _, err := fmt.Fprintf(w, "Checking Turnbeam (installation: %s)\n", displayDir(dir)); err != nil {
			return err
		}
	}
	checks, err := service.Doctor(cmd.Context())
	if err == nil || errors.Is(err, setup.ErrDoctorFailed) {
		checks = append(checks, keyCheck(cmd.Context(), keys))
	}
	failed := false
	for _, check := range checks {
		failed = failed || !check.OK
	}
	if err == nil && failed {
		err = setup.ErrDoctorFailed
	}
	if writeErr := printChecks(w, checks, quiet, color); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	if err != nil || quiet {
		return err
	}
	ready := "Ready to transcribe."
	if color {
		ready = ansiGreen + ready + ansiReset
	}
	_, err = fmt.Fprintln(w, ready)
	return err
}

func keyCheck(ctx context.Context, keys keyStatus) setup.Check {
	check := setup.Check{Name: "Meta API key"}
	source, available, err := keys(ctx)
	switch {
	case err != nil:
		check.Message, check.Fix = "could not be read", "unlock your login keychain, or set META_API_KEY"
	case !available:
		check.Message, check.Fix = "not configured", "run `turnbeam key set`, or set META_API_KEY"
	case source == credentials.Environment:
		check.Message, check.OK = "from the META_API_KEY environment variable", true
	default:
		check.Message, check.OK = "stored in Keychain", true
	}
	return check
}

// printChecks writes an aligned checklist. Terminals get colored ✓/✗ marks;
// other output keeps greppable OK/FAIL labels. Quiet mode lists failures only.
func printChecks(w io.Writer, checks []setup.Check, quiet, color bool) error {
	width := 0
	for _, check := range checks {
		width = max(width, len(check.Name))
	}
	var out strings.Builder
	for _, check := range checks {
		if quiet && check.OK {
			continue
		}
		mark, pad, arrow := "OK  ", "    ", "->"
		if !check.OK {
			mark = "FAIL"
		}
		if color {
			mark, pad, arrow = ansiGreen+"✓"+ansiReset, " ", ansiYellow+"→"+ansiReset
			if !check.OK {
				mark = ansiRed + "✗" + ansiReset
			}
		}
		name := fmt.Sprintf("%-*s", width, check.Name)
		if color {
			name = ansiBold + name + ansiReset
		}
		fmt.Fprintf(&out, "  %s %s  %s\n", mark, name, check.Message)
		if !check.OK && check.Fix != "" {
			fmt.Fprintf(&out, "  %s %*s  %s %s\n", pad, width, "", arrow, check.Fix)
		}
	}
	_, err := io.WriteString(w, out.String())
	return err
}
