//go:build !darwin && !linux

package progress

import "io"

func terminal(io.Writer) bool { return false }
