//go:build !windows

package updater

import "syscall"

// replaceProcess overlays the current process image with the new binary, so the
// PID survives the update and any supervisor stays unaware of the restart.
func replaceProcess(path string, args, env []string) error {
	return syscall.Exec(path, args, env)
}
