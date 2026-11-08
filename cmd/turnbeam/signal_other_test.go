//go:build !darwin && !linux

package main

import (
	"context"
	"testing"
)

func testSignals(t *testing.T, _ context.Context, _ string) { t.Skip("requires POSIX signals") }
