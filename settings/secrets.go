package settings

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

var ErrSecretUnavailable = errors.New("secret not available")

// ResolveSecret turns a secret field from config.json into its value:
//
//	"env:NAME"  the environment variable NAME
//	"file:PATH" the contents of the file at PATH (e.g. a Docker secret),
//	            surrounding whitespace trimmed
//	anything else, the value itself
//
// config.json keeps the reference, never the resolved value, so a secret
// can live outside the file. An unset variable, a missing file or an empty
// value is an error wrapping ErrSecretUnavailable. Errors name the variable
// or file, never the value.
func ResolveSecret(reference string, getenv func(string) string) (string, error) {
	switch {
	case strings.HasPrefix(reference, "env:"):
		name := strings.TrimPrefix(reference, "env:")
		if name == "" {
			return "", fmt.Errorf("%w: env: reference without a variable name", ErrSecretUnavailable)
		}
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			return "", fmt.Errorf("%w: environment variable %s is not set", ErrSecretUnavailable, name)
		}
		return value, nil
	case strings.HasPrefix(reference, "file:"):
		path := strings.TrimPrefix(reference, "file:")
		if path == "" {
			return "", fmt.Errorf("%w: file: reference without a path", ErrSecretUnavailable)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			// The OS error names the path only.
			return "", fmt.Errorf("%w: %w", ErrSecretUnavailable, err)
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", fmt.Errorf("%w: file %s is empty", ErrSecretUnavailable, path)
		}
		return value, nil
	default:
		value := strings.TrimSpace(reference)
		if value == "" {
			return "", fmt.Errorf("%w: empty value", ErrSecretUnavailable)
		}
		return value, nil
	}
}
