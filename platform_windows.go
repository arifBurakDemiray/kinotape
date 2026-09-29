//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// detach starts the server without a console so it outlives the launcher.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup, HideWindow: true}
}

// hideWindow keeps ffmpeg from flashing a console window.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
}

// openBrowser opens the UI in a Chrome or Edge app window, or the default browser.
func openBrowser(url string) error {
	roots := []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")}
	for _, rel := range []string{`Google\Chrome\Application\chrome.exe`, `Microsoft\Edge\Application\msedge.exe`,
		`BraveSoftware\Brave-Browser\Application\brave.exe`} {
		for _, root := range roots {
			if root == "" {
				continue
			}
			if exe := filepath.Join(root, rel); fileExists(exe) {
				return exec.Command(exe, "--app="+url).Start()
			}
		}
	}
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// pickFolder shows the Windows folder picker and returns the chosen path, or "" when cancelled.
func pickFolder() (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms;` +
		`$owner = New-Object System.Windows.Forms.Form -Property @{TopMost=$true};` +
		`$dialog = New-Object System.Windows.Forms.FolderBrowserDialog;` +
		`$dialog.Description = 'Choose a folder with videos';` +
		`if ($dialog.ShowDialog($owner) -eq 'OK') { $dialog.SelectedPath }`
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-NonInteractive", "-Command", script)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// openExternal opens a file or UNC path with its default app.
func openExternal(target string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
}
