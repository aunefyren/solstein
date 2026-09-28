//go:build unix

package console

import (
	"fmt"
	"os"
	"syscall"
)

// runAsOwnerOf switches a process running as root to the owner of dir (the
// config directory), as the entrypoint does for the app: files SQLite
// creates (its WAL and shared-memory files) then belong to the app's user.
// Not root, or the directory root's own, it changes nothing.
//
// It also sets the entrypoint's umask (027), which `docker exec` skips: a
// database file the console creates is then no more readable than the app's.
func runAsOwnerOf(dir string) error {
	syscall.Umask(0o027)
	if os.Geteuid() != 0 {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid == 0 {
		return nil
	}
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("drop groups: %w", err)
	}
	if err := syscall.Setgid(int(stat.Gid)); err != nil {
		return fmt.Errorf("set group %d: %w", stat.Gid, err)
	}
	if err := syscall.Setuid(int(stat.Uid)); err != nil {
		return fmt.Errorf("set user %d: %w", stat.Uid, err)
	}
	return nil
}
