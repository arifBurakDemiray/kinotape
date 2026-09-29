//go:build darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// detach starts the server in its own session so it outlives the launcher.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// hideWindow does nothing on macOS, where tools have no console window.
func hideWindow(*exec.Cmd) {}

// openBrowser opens the UI in a Chromium browser's app window, or the default browser.
func openBrowser(url string) error {
	home, _ := os.UserHomeDir()
	for _, name := range []string{"Google Chrome", "Microsoft Edge", "Brave Browser", "Chromium", "Vivaldi"} {
		for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
			if _, err := os.Stat(filepath.Join(dir, name+".app")); err == nil {
				return exec.Command("open", "-na", name, "--args", "--app="+url).Run()
			}
		}
	}
	return exec.Command("open", url).Run()
}

// pickFolder shows the macOS folder picker and returns the chosen path, or "" when cancelled.
func pickFolder() (string, error) {
	out, err := exec.Command("osascript", "-e", "tell me to activate",
		"-e", `POSIX path of (choose folder with prompt "Choose a folder with videos")`).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(strings.TrimSpace(string(out)), "/"), nil
}

// openExternal opens a file, or a share file as an smb:// URL, with the default app.
func openExternal(target string) error {
	if strings.HasPrefix(target, `\\`) {
		target = "smb:" + strings.ReplaceAll(target, `\`, "/")
	}
	return exec.Command("open", target).Run()
}
