// Package nativeenv isolates managed native processes from Meta credentials and overrides.
package nativeenv

import "strings"

// Sanitize removes Meta credentials and native loader, backend and configuration overrides.
func Sanitize(environment []string) []string {
	clean := make([]string, 0, len(environment))
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "META_") ||
			strings.HasPrefix(name, "DYLD_") ||
			strings.HasPrefix(name, "LD_") ||
			strings.HasPrefix(name, "NEMO_") ||
			strings.HasPrefix(name, "GGML_") ||
			strings.HasPrefix(name, "LLAMA_") {
			continue
		}
		clean = append(clean, value)
	}
	return clean
}
