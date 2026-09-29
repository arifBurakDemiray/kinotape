// Kinotape is a Netflix-style library for video folders on this computer and on SMB shares.
//
// Running it with no arguments opens the library in a browser app window, starting the background server when
// it is not running yet. "kinotape serve" runs the server in the foreground and "kinotape stop" stops it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	appName     = "Kinotape"
	defaultPort = 8765
)

// runtimeInfo tells a launcher where the running server listens.
type runtimeInfo struct {
	Port int `json:"port"`
	PID  int `json:"pid"`
}

// main dispatches the command line.
func main() {
	command := "open"
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-psn") {
		command = os.Args[1]
	}
	var err error
	switch command {
	case "open":
		err = openApp()
	case "serve":
		err = serve()
	case "stop":
		err = stopServer()
	default:
		err = fmt.Errorf("usage: kinotape [open|serve|stop]")
	}
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

// appDirs returns the folders for settings and for caches.
func appDirs() (config, cache string) {
	base, err := os.UserConfigDir()
	if err != nil {
		base, _ = os.UserHomeDir()
	}
	caches, err := os.UserCacheDir()
	if err != nil {
		caches = base
	}
	return filepath.Join(base, appName), filepath.Join(caches, appName)
}

// runtimeFile is where the server records its port.
func runtimeFile() string {
	config, _ := appDirs()
	return filepath.Join(config, "server.json")
}

// serve runs the library server until it is told to quit.
func serve() error {
	configDir, cacheDir := appDirs()
	store, err := openStore(configDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	listener, err := listenLocal()
	if err != nil {
		return err
	}
	app := &App{store: store, cacheDir: cacheDir, port: listener.Addr().(*net.TCPAddr).Port, ffmpeg: findTool("ffmpeg"),
		ffprobe: findTool("ffprobe"), quit: make(chan struct{}), thumbGate: make(chan struct{}, 3)}
	app.lib = newLibrary(app)
	app.lib.Configure(store.Config())
	server := &http.Server{Handler: app.routes(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Println(err)
		}
	}()
	if err := writeJSON(runtimeFile(), runtimeInfo{Port: app.port, PID: os.Getpid()}); err != nil {
		return err
	}
	// Scanning can wait on a macOS privacy prompt for folders like Downloads, so it must not hold up the launcher.
	go app.lib.ScanAll(true, false)
	defer os.Remove(runtimeFile())
	log.Printf("%s is running at http://127.0.0.1:%d/ (ffmpeg: %q)", appName, app.port, app.ffmpeg)

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	for {
		select {
		case <-ticker.C:
			app.lib.ScanAll(false, false)
		case <-signals:
			return shutdown(server)
		case <-app.quit:
			return shutdown(server)
		}
	}
}

// shutdown stops the HTTP server, letting open requests finish briefly.
func shutdown(server *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return server.Shutdown(ctx)
}

// listenLocal binds to localhost on the default port, or the next free one.
func listenLocal() (net.Listener, error) {
	if value := os.Getenv("KINOTAPE_PORT"); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("KINOTAPE_PORT must be a number")
		}
		return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	}
	var lastErr error
	for port := defaultPort; port < defaultPort+20; port++ {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return listener, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// runningPort returns the port of a server that answers, or 0.
func runningPort() int {
	var info runtimeInfo
	if err := readJSON(runtimeFile(), &info); err != nil || info.Port == 0 {
		return 0
	}
	client := &http.Client{Timeout: 700 * time.Millisecond}
	res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", info.Port))
	if err != nil {
		return 0
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0
	}
	return info.Port
}

// openApp opens the library window, starting the background server first when needed.
func openApp() error {
	port := runningPort()
	if port == 0 {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		configDir, _ := appDirs()
		if err := os.MkdirAll(configDir, 0o755); err != nil {
			return err
		}
		logPath := filepath.Join(configDir, "kinotape.log")
		if info, err := os.Stat(logPath); err == nil && info.Size() > 5<<20 {
			_ = os.Remove(logPath)
		}
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		cmd := exec.Command(self, "serve")
		cmd.Stdout, cmd.Stderr = logFile, logFile
		detach(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		_ = cmd.Process.Release()
		logFile.Close()
		for i := 0; i < 150 && port == 0; i++ {
			time.Sleep(100 * time.Millisecond)
			port = runningPort()
		}
		if port == 0 {
			return fmt.Errorf("%s did not start; see %s", appName, logPath)
		}
	}
	return openBrowser(fmt.Sprintf("http://127.0.0.1:%d/", port))
}

// stopServer asks a running server to quit.
func stopServer() error {
	port := runningPort()
	if port == 0 {
		return nil
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/quit", port), strings.NewReader("{}"))
	req.Header.Set("X-Kinotape", "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return res.Body.Close()
}
