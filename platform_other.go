//go:build !darwin && !windows

package main

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
)

// detach starts the server in its own session so it outlives the launcher.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// hideWindow does nothing where tools have no console window.
func hideWindow(*exec.Cmd) {}

// openBrowser opens the UI in a Chromium browser's app window, or the default browser.
func openBrowser(url string) error {
	for _, name := range []string{"google-chrome", "microsoft-edge", "brave-browser", "chromium", "chromium-browser"} {
		if exe, err := exec.LookPath(name); err == nil {
			return exec.Command(exe, "--app="+url).Start()
		}
	}
	return exec.Command("xdg-open", url).Start()
}

// pickFolder shows a folder picker through zenity, when it is installed.
func pickFolder() (string, error) {
	out, err := exec.Command("zenity", "--file-selection", "--directory", "--title=Choose a folder with videos").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", nil
	}
	return strings.TrimSpace(string(out)), err
}

// openExternal opens a file with the default app.
func openExternal(target string) error {
	return exec.Command("xdg-open", target).Start()
}
