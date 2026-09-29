package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui
var embedded embed.FS

// App ties the settings, library, tools and HTTP server together.
type App struct {
	store       *Store
	lib         *Library
	cacheDir    string
	port        int
	ffmpeg      string
	ffprobe     string
	quit        chan struct{}
	quitOnce    sync.Once
	thumbGate   chan struct{}
	thumbFailed sync.Map
	locks       keyedLocks
	ui          fs.FS
}

var (
	showIDRe  = regexp.MustCompile(`^[a-z0-9-]+$`)
	subFileRe = regexp.MustCompile(`^(\d+)\.(vtt|ass)$`)
)

// routes builds the HTTP handler. KINOTAPE_UI_DIR serves the page from disk instead of the built-in copy.
func (a *App) routes() http.Handler {
	a.ui, _ = fs.Sub(embedded, "ui")
	if dir := os.Getenv("KINOTAPE_UI_DIR"); dir != "" {
		a.ui = os.DirFS(dir)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.Handle("GET /vendor/", http.FileServerFS(a.ui))
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) { writeJSONResponse(w, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /api/library", a.handleLibrary)
	mux.HandleFunc("POST /api/rescan", a.handleRescan)
	mux.HandleFunc("GET /video/{id}", a.handleVideo)
	mux.HandleFunc("GET /thumb/{id}", a.handleThumb)
	mux.HandleFunc("GET /subs/{id}/{file}", a.handleSubtitle)
	mux.HandleFunc("GET /fonts/{id}/{name}", a.handleFont)
	mux.HandleFunc("GET /art/{show}", a.handleArt)
	mux.HandleFunc("POST /api/progress", a.handleProgress)
	mux.HandleFunc("POST /api/mark", a.handleMark)
	mux.HandleFunc("POST /api/settings", a.handleSettings)
	mux.HandleFunc("GET /api/discover", a.handleDiscover)
	mux.HandleFunc("POST /api/smb/shares", a.handleShares)
	mux.HandleFunc("POST /api/smb/dirs", a.handleShareDirs)
	mux.HandleFunc("POST /api/sources/add", a.handleAddSource)
	mux.HandleFunc("POST /api/sources/remove", a.handleRemoveSource)
	mux.HandleFunc("POST /api/sources/pick", a.handlePickFolder)
	mux.HandleFunc("POST /api/open-external", a.handleOpenExternal)
	mux.HandleFunc("POST /api/quit", a.handleQuit)
	return a.guard(mux)
}

// guard only serves requests addressed to this machine, requires Kinotape's header on changes, and isolates
// the page from other origins so the subtitle renderer can run on several threads.
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("X-Kinotape") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
		next.ServeHTTP(w, r)
	})
}

// handleIndex serves the single-page UI.
func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	page, err := fs.ReadFile(a.ui, "index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// handleLibrary returns the whole library view, rescanning local folders when they are stale.
func (a *App) handleLibrary(w http.ResponseWriter, r *http.Request) {
	a.lib.ScanAll(false, false)
	writeJSONResponse(w, a.lib.View(a.store.Config(), a.store.State(), runtime.GOOS))
}

// handleRescan scans every source again.
func (a *App) handleRescan(w http.ResponseWriter, r *http.Request) {
	a.lib.ScanAll(true, true)
	writeJSONResponse(w, map[string]bool{"ok": true})
}

// handleVideo streams a file with range support so the player can seek.
func (a *App) handleVideo(w http.ResponseWriter, r *http.Request) {
	e := a.lib.Entry(r.PathValue("id"))
	if e == nil {
		http.NotFound(w, r)
		return
	}
	file, err := a.lib.Open(r.Context(), e)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", videoType(e.Rel))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", e.ModTime, file)
}

// handleThumb serves a cached still, grabbing one with ffmpeg on first request.
func (a *App) handleThumb(w http.ResponseWriter, r *http.Request) {
	e := a.lib.Entry(r.PathValue("id"))
	if e == nil || a.ffmpeg == "" {
		http.NotFound(w, r)
		return
	}
	out := filepath.Join(a.cacheDir, "thumbs", fmt.Sprintf("%s-%d.jpg", e.ID, e.Size))
	if !fileExists(out) {
		if failed, ok := a.thumbFailed.Load(out); ok && time.Since(failed.(time.Time)) < 5*time.Minute {
			http.NotFound(w, r)
			return
		}
		a.thumbGate <- struct{}{}
		lock := a.locks.get(out)
		lock.Lock()
		if !fileExists(out) {
			duration := 0.0
			if m := a.lib.Media(e); m != nil {
				duration = m.Duration
			}
			_ = os.MkdirAll(filepath.Dir(out), 0o755)
			if err := makeThumbnail(a.ffmpeg, a.lib.Input(e), out, duration); err != nil {
				a.thumbFailed.Store(out, time.Now())
			}
		}
		lock.Unlock()
		<-a.thumbGate
	}
	serveCached(w, r, out, "image/jpeg")
}

// handleSubtitle serves one subtitle stream as WebVTT or ASS, extracting it on first request.
func (a *App) handleSubtitle(w http.ResponseWriter, r *http.Request) {
	e := a.lib.Entry(r.PathValue("id"))
	m := subFileRe.FindStringSubmatch(r.PathValue("file"))
	if e == nil || m == nil || a.ffmpeg == "" {
		http.NotFound(w, r)
		return
	}
	index, _ := strconv.Atoi(m[1])
	format := m[2]
	media := a.lib.Media(e)
	var stream *Stream
	if media != nil {
		for i := range media.Subtitles {
			if media.Subtitles[i].Index == index {
				stream = &media.Subtitles[i]
			}
		}
	}
	if stream == nil {
		http.NotFound(w, r)
		return
	}
	style := "plain"
	if format == "ass" {
		style = md5Hex(srtStyle)[:6]
	}
	out := filepath.Join(a.cacheDir, "subtitles", fmt.Sprintf("%s-%d-%d-%s.%s", e.ID, e.Size, index, style, format))
	lock := a.locks.get(out)
	lock.Lock()
	if !fileExists(out) {
		_ = os.MkdirAll(filepath.Dir(out), 0o755)
		if err := extractSubtitle(a.ffmpeg, a.lib.Input(e), out, *stream, format); err != nil {
			log.Printf("subtitles %s #%d: %v", e.Rel, index, err)
		}
	}
	lock.Unlock()
	contentType := "text/plain; charset=utf-8"
	if format == "vtt" {
		contentType = "text/vtt; charset=utf-8"
	}
	serveCached(w, r, out, contentType)
}

// handleFont serves one font attached to a video, extracting them all on first request.
func (a *App) handleFont(w http.ResponseWriter, r *http.Request) {
	e := a.lib.Entry(r.PathValue("id"))
	name := r.PathValue("name")
	media := (*Media)(nil)
	if e != nil {
		media = a.lib.Media(e)
	}
	if media == nil || a.ffmpeg == "" || !contains(media.Fonts, name) {
		http.NotFound(w, r)
		return
	}
	dir := filepath.Join(a.cacheDir, "fonts", fmt.Sprintf("%s-%d", e.ID, e.Size))
	lock := a.locks.get(dir)
	lock.Lock()
	if !fileExists(filepath.Join(dir, ".done")) {
		if err := extractFonts(a.ffmpeg, a.lib.Input(e), dir); err != nil {
			log.Printf("fonts %s: %v", e.Rel, err)
		}
	}
	lock.Unlock()
	contentType := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	serveCached(w, r, filepath.Join(dir, name), contentType)
}

// handleArt serves a cached poster.
func (a *App) handleArt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("show")
	if !showIDRe.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	serveCached(w, r, filepath.Join(a.cacheDir, "art", id+".jpg"), "image/jpeg")
}

// handleProgress remembers how far the player got.
func (a *App) handleProgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string  `json:"id"`
		Position float64 `json:"position"`
	}
	if !readBody(w, r, &body) {
		return
	}
	e := a.lib.Entry(body.ID)
	if e == nil {
		writeError(w, http.StatusNotFound, "That video is no longer in the library.")
		return
	}
	position := clampPosition(body.Position)
	if m := a.lib.Media(e); m != nil && m.Duration > 0 && position > m.Duration {
		position = m.Duration
	}
	if err := a.store.SetProgress(e.ID, position); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONResponse(w, map[string]bool{"ok": true})
}

// handleMark stores a watched mark, dropping it when it matches what the progress already shows.
func (a *App) handleMark(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		Watched *bool  `json:"watched"`
	}
	if !readBody(w, r, &body) {
		return
	}
	view := a.lib.View(a.store.Config(), a.store.State(), runtime.GOOS)
	var episode *EpisodeView
	for _, show := range view.Shows {
		for _, e := range show.Episodes {
			if e.ID == body.ID {
				episode = e
			}
		}
	}
	if episode == nil {
		writeError(w, http.StatusNotFound, "That video is no longer in the library.")
		return
	}
	watched := body.Watched
	if watched != nil && *watched == episode.AutoWatched {
		watched = nil
	}
	if err := a.store.SetMark(body.ID, watched); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONResponse(w, map[string]bool{"ok": true})
}

// handleSettings saves playback and artwork settings.
func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Autoplay *bool   `json:"autoplay"`
		Artwork  *bool   `json:"artwork"`
		TMDBKey  *string `json:"tmdb_key"`
	}
	if !readBody(w, r, &body) {
		return
	}
	keyChanged := false
	err := a.store.UpdateConfig(func(c *Config) {
		if body.Autoplay != nil {
			c.Autoplay = *body.Autoplay
		}
		if body.Artwork != nil {
			c.Artwork = *body.Artwork
		}
		if body.TMDBKey != nil && strings.TrimSpace(*body.TMDBKey) != c.TMDBKey {
			c.TMDBKey = strings.TrimSpace(*body.TMDBKey)
			keyChanged = true
		}
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if keyChanged {
		a.lib.ForgetMeta()
	}
	writeJSONResponse(w, map[string]bool{"ok": true})
}

// handleDiscover looks for SMB servers on the local network.
func (a *App) handleDiscover(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	servers := discoverServers(ctx)
	if servers == nil {
		servers = []Server{}
	}
	writeJSONResponse(w, servers)
}

// smbLogin is the connection part of a share request.
type smbLogin struct {
	Host     string `json:"host"`
	User     string `json:"user"`
	Password string `json:"password"`
	Domain   string `json:"domain"`
	Share    string `json:"share"`
	Dir      string `json:"dir"`
}

// handleShares logs in to a server and lists its shares.
func (a *App) handleShares(w http.ResponseWriter, r *http.Request) {
	var body smbLogin
	if !readBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Host) == "" {
		writeError(w, http.StatusBadRequest, "Enter the server's name or address.")
		return
	}
	shares, err := listShares(strings.TrimSpace(body.Host), body.User, body.Password, body.Domain)
	if err != nil {
		writeError(w, http.StatusBadGateway, friendlySMBError(err))
		return
	}
	if shares == nil {
		shares = []ShareInfo{}
	}
	writeJSONResponse(w, shares)
}

// handleShareDirs lists the folders inside a share folder.
func (a *App) handleShareDirs(w http.ResponseWriter, r *http.Request) {
	var body smbLogin
	if !readBody(w, r, &body) {
		return
	}
	dirs, err := listShareDirs(strings.TrimSpace(body.Host), body.User, body.Password, body.Domain, body.Share, body.Dir)
	if err != nil {
		writeError(w, http.StatusBadGateway, friendlySMBError(err))
		return
	}
	if dirs == nil {
		dirs = []string{}
	}
	writeJSONResponse(w, dirs)
}

// handleAddSource adds a local folder or an SMB share to the library.
func (a *App) handleAddSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
		Path string `json:"path"`
		smbLogin
	}
	if !readBody(w, r, &body) {
		return
	}
	var src Source
	switch body.Kind {
	case "local":
		folder := filepath.Clean(expandHome(strings.TrimSpace(body.Path)))
		if info, err := os.Stat(folder); err != nil || !info.IsDir() {
			writeError(w, http.StatusBadRequest, "There is no folder at "+folder+".")
			return
		}
		src = Source{ID: "local-" + md5Hex(folder)[:8], Kind: "local", Path: folder}
	case "smb":
		host := strings.TrimSpace(body.Host)
		dir := strings.Trim(strings.ReplaceAll(body.Dir, `\`, "/"), "/")
		src = Source{Kind: "smb", Host: host, Share: body.Share, Dir: dir, User: body.User, Domain: body.Domain}
		src.ID = "smb-" + md5Hex(strings.ToLower(host) + "|" + body.Share + "|" + dir + "|" + body.User)[:8]
		if _, err := listShareDirs(host, body.User, body.Password, body.Domain, body.Share, dir); err != nil {
			writeError(w, http.StatusBadGateway, friendlySMBError(err))
			return
		}
		if err := a.store.SetSecret(src.ID, body.Password); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "Unknown source type.")
		return
	}
	err := a.store.UpdateConfig(func(c *Config) {
		for _, existing := range c.Sources {
			if existing.ID == src.ID {
				return
			}
		}
		c.Sources = append(c.Sources, src)
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.lib.Configure(a.store.Config())
	a.lib.ScanAll(false, true)
	writeJSONResponse(w, map[string]string{"id": src.ID})
}

// handleRemoveSource takes a source out of the library and forgets its password.
func (a *App) handleRemoveSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !readBody(w, r, &body) {
		return
	}
	err := a.store.UpdateConfig(func(c *Config) {
		kept := c.Sources[:0]
		for _, src := range c.Sources {
			if src.ID != body.ID {
				kept = append(kept, src)
			}
		}
		c.Sources = kept
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.store.DeleteSecret(body.ID)
	a.lib.Configure(a.store.Config())
	writeJSONResponse(w, map[string]bool{"ok": true})
}

// handlePickFolder shows the system folder picker.
func (a *App) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	folder, err := pickFolder()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "The folder picker could not open. Paste the folder's path instead.")
		return
	}
	writeJSONResponse(w, map[string]string{"path": folder})
}

// handleOpenExternal opens a file in the system's default video player.
func (a *App) handleOpenExternal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !readBody(w, r, &body) {
		return
	}
	e := a.lib.Entry(body.ID)
	if e == nil {
		writeError(w, http.StatusNotFound, "That video is no longer in the library.")
		return
	}
	target := a.lib.ExternalPath(e)
	if target == "" {
		writeError(w, http.StatusBadRequest, "This file can't be opened in another player.")
		return
	}
	if err := openExternal(target); err != nil {
		writeError(w, http.StatusInternalServerError, "Your video player could not open the file.")
		return
	}
	writeJSONResponse(w, map[string]bool{"ok": true})
}

// handleQuit stops the background server.
func (a *App) handleQuit(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, map[string]bool{"ok": true})
	a.quitOnce.Do(func() { close(a.quit) })
}

// videoType is the content type the browser expects for a video file.
func videoType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".mp4", ".m4v":
		return "video/mp4"
	}
	return "application/octet-stream"
}

// serveCached sends a cache file that never changes under the same URL.
func serveCached(w http.ResponseWriter, r *http.Request, file, contentType string) {
	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "max-age=604800, immutable")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// readBody decodes a JSON request body, answering 400 when it is malformed.
func readBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "Kinotape could not read that request.")
		return false
	}
	return true
}

// writeJSONResponse sends a JSON answer that is never cached.
func writeJSONResponse(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

// writeError sends a JSON error the UI can show as is.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// friendlySMBError turns an SMB failure into a sentence someone can act on.
func friendlySMBError(err error) string {
	text := strings.ToLower(err.Error())
	var netErr net.Error
	switch {
	case strings.Contains(text, "logon") || strings.Contains(text, "password") || strings.Contains(text, "access denied") ||
		strings.Contains(text, "access_denied") || strings.Contains(text, "status_logon_failure"):
		return "The server refused that user name or password."
	case strings.Contains(text, "bad_network_name") || strings.Contains(text, "not found"):
		return "The server has no share or folder with that name."
	case errors.As(err, &netErr) && netErr.Timeout(), strings.Contains(text, "timeout"), strings.Contains(text, "deadline"):
		return "The server did not answer. Check the address and that file sharing is on."
	case strings.Contains(text, "refused") || strings.Contains(text, "no route") || strings.Contains(text, "no such host"):
		return "Kinotape could not reach that server. Check the address and that file sharing is on."
	}
	return "The share could not be opened: " + err.Error()
}

// clampPosition turns a position that is not a finite, non-negative number into zero.
func clampPosition(f float64) float64 {
	if math.IsNaN(f) || f < 0 || f > 1e9 {
		return 0
	}
	return f
}
