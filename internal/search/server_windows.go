//go:build windows

package search

import (
	"os/exec"
	"syscall"
)

// hideConsole keeps the sidecar from flashing a console window. The app is a
// windowsgui binary, so Windows would otherwise allocate a brand-new console
// for the runtime process.
const createNoWindow = 0x08000000

func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
