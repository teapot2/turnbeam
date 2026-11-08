// Package media prepares local recordings without buffering audio in Go memory.
package media

import (
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/teapot2/turnbeam/internal/limitbuf"
	"github.com/teapot2/turnbeam/internal/subprocess"
)

// Processor uses FFmpeg and FFprobe from PATH unless explicit paths are supplied.
// TempDir defaults to the system temporary directory.
type Processor struct {
	FFmpeg  string
	FFprobe string
	TempDir string
	command func(context.Context, string, ...string) *exec.Cmd
}

func (p Processor) run(ctx context.Context, tool, name string, args ...string) ([]byte, error) {
	output := limitbuf.Buffer{Limit: 1 << 20}
	if err := p.runTo(ctx, tool, name, &output, args...); err != nil {
		return nil, err
	}
	if output.Overflow {
		return nil, fmt.Errorf("%s metadata exceeds the 1 MiB limit", name)
	}
	return output.Bytes(), nil
}

// runTo streams the tool's stdout to w, which must keep accepting writes so the
// child never blocks on a full pipe.
func (p Processor) runTo(ctx context.Context, tool, name string, w io.Writer, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tool == "" {
		tool = name
	}
	command := p.command
	if command == nil {
		command = exec.CommandContext
	}
	cmd := command(ctx, tool, args...)
	cmd.Stdout = w
	// Native diagnostics can contain private filenames and media metadata.
	cmd.Stderr = io.Discard
	if err := subprocess.Run(ctx, cmd); err != nil {
		return fmt.Errorf("%s failed (check installation and decoder support): %w", name, err)
	}
	return nil
}
