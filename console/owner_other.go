//go:build !unix

package console

// runAsOwnerOf has nothing to do outside Unix: there is no root to leave.
func runAsOwnerOf(string) error { return nil }
