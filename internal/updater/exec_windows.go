//go:build windows

package updater

import "os"

// replaceProcess has no exec equivalent on Windows: start a fresh process and
// exit. Only reachable as a fallback; applyUpdateWindows drives the real swap.
func replaceProcess(path string, args, env []string) error {
	proc, err := os.StartProcess(path, args, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Env:   env,
	})
	if err != nil {
		return err
	}
	_ = proc.Release()
	os.Exit(0)
	return nil
}
