package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	resumeAfter      = 30.0
	localRescanAfter = 30 * time.Second
	shareRescanAfter = 10 * time.Minute
)

// Entry is one video file in the library.
type Entry struct {
	ID       string
	SourceID string
	Rel      string
	Size     int64
	ModTime  time.Time
	Parsed   Parsed
}

// sourceRuntime is a configured source with its backend and latest scan.
type sourceRuntime struct {
	src      Source
	backend  Backend
	entries  map[string]*Entry
	scanned  time.Time
	scanning bool
	err      string
}

// probeRecord caches what ffprobe found for one file version.
type probeRecord struct {
	Sig   string `json:"sig"`
	Media *Media `json:"media,omitempty"`
}

// metaJob asks the catalogue worker to look up one show.
type metaJob struct {
	id, title, kind string
	year            int
	anime           bool
}

// Library keeps every source's files together with media details and catalogue metadata.
type Library struct {
	app         *App
	mu          sync.RWMutex
	sources     map[string]*sourceRuntime
	order       []string
	probes      map[string]probeRecord
	probing     map[string]bool
	metas       map[string]Meta
	metaPending map[string]bool
	probeJobs   chan string
	metaJobs    chan metaJob
	dirty       chan string
}

// newLibrary loads the caches and starts the background workers.
func newLibrary(app *App) *Library {
	l := &Library{
		app: app, sources: map[string]*sourceRuntime{}, probes: map[string]probeRecord{}, probing: map[string]bool{},
		metas: map[string]Meta{}, metaPending: map[string]bool{},
		probeJobs: make(chan string, 4096), metaJobs: make(chan metaJob, 1024), dirty: make(chan string, 16),
	}
	_ = readJSON(filepath.Join(app.cacheDir, "probe.json"), &l.probes)
	_ = readJSON(filepath.Join(app.cacheDir, "meta.json"), &l.metas)
	for i := 0; i < 3; i++ {
		go l.probeWorker()
	}
	go l.metaWorker()
	go l.saver()
	return l
}

// Configure brings the running sources in line with the settings.
func (l *Library) Configure(cfg Config) {
	l.mu.Lock()
	keep := map[string]bool{}
	l.order = l.order[:0]
	for _, src := range cfg.Sources {
		keep[src.ID] = true
		l.order = append(l.order, src.ID)
		if rt := l.sources[src.ID]; rt != nil && rt.src == src {
			continue
		} else if rt != nil {
			rt.backend.Close()
		}
		l.sources[src.ID] = &sourceRuntime{src: src, backend: newBackend(src, l.app.store.Secret(src.ID)), entries: map[string]*Entry{}}
	}
	for id, rt := range l.sources {
		if !keep[id] {
			rt.backend.Close()
			delete(l.sources, id)
		}
	}
	l.mu.Unlock()
}

// ScanAll rescans sources whose last scan is old; local folders are scanned before returning when wait is set.
func (l *Library) ScanAll(force, wait bool) {
	l.mu.RLock()
	var due []*sourceRuntime
	for _, id := range l.order {
		rt := l.sources[id]
		limit := localRescanAfter
		if rt.src.Kind == "smb" {
			limit = shareRescanAfter
		}
		if !rt.scanning && (force || time.Since(rt.scanned) > limit) {
			due = append(due, rt)
		}
	}
	l.mu.RUnlock()
	var wg sync.WaitGroup
	for _, rt := range due {
		if wait && rt.src.Kind != "smb" {
			wg.Add(1)
			go func(rt *sourceRuntime) { defer wg.Done(); l.scan(rt) }(rt)
		} else {
			go l.scan(rt)
		}
	}
	wg.Wait()
}

// scan walks one source and replaces its file list.
func (l *Library) scan(rt *sourceRuntime) {
	l.mu.Lock()
	if rt.scanning {
		l.mu.Unlock()
		return
	}
	rt.scanning = true
	l.mu.Unlock()

	found := map[string]*Entry{}
	err := rt.backend.Walk(func(rel string, size int64, mod time.Time) {
		e := &Entry{ID: l.entryID(rt, rel), SourceID: rt.src.ID, Rel: rel, Size: size, ModTime: mod, Parsed: parseVideo(rel)}
		found[e.ID] = e
	})

	l.mu.Lock()
	rt.scanning = false
	rt.scanned = time.Now()
	if err != nil {
		rt.err = err.Error()
		log.Printf("scan %s: %v", rt.src.ID, err)
	} else {
		rt.err = ""
		rt.entries = found
	}
	var queue []string
	for id, e := range rt.entries {
		if rec, ok := l.probes[id]; (!ok || rec.Sig != probeSig(e)) && !l.probing[id] {
			l.probing[id] = true
			queue = append(queue, id)
		}
	}
	l.mu.Unlock()
	if l.app.ffprobe == "" {
		return
	}
	for _, id := range queue {
		select {
		case l.probeJobs <- id:
		default:
		}
	}
}

// entryID gives a file a stable id; local files use the md5 of their path.
func (l *Library) entryID(rt *sourceRuntime, rel string) string {
	if local := rt.backend.LocalPath(rel); local != "" {
		return md5Hex(local)
	}
	return md5Hex("smb://" + strings.ToLower(rt.src.Host) + "/" + rt.src.Share + "/" + strings.Trim(path.Join(rt.src.Dir, rel), "/"))
}

// probeSig identifies one version of a file for the probe cache.
func probeSig(e *Entry) string {
	return fmt.Sprintf("%d-%d-%d", e.Size, e.ModTime.Unix(), probeVersion)
}

// probeWorker runs ffprobe on files waiting in the queue.
func (l *Library) probeWorker() {
	for id := range l.probeJobs {
		e := l.Entry(id)
		if e == nil {
			l.mu.Lock()
			delete(l.probing, id)
			l.mu.Unlock()
			continue
		}
		media, err := probeMedia(l.app.ffprobe, l.Input(e))
		if err != nil {
			log.Printf("probe %s: %v", e.Rel, err)
		}
		l.mu.Lock()
		l.probes[id] = probeRecord{Sig: probeSig(e), Media: media}
		delete(l.probing, id)
		l.mu.Unlock()
		l.markDirty("probe")
	}
}

// metaWorker looks shows up in the online catalogues one at a time, which keeps within their rate limits.
func (l *Library) metaWorker() {
	for job := range l.metaJobs {
		cfg := l.app.store.Config()
		meta := fetchMeta(job.title, job.year, job.kind, job.anime, cfg.TMDBKey, filepath.Join(l.app.cacheDir, "art", job.id+".jpg"))
		l.mu.Lock()
		l.metas[job.id] = meta
		delete(l.metaPending, job.id)
		l.mu.Unlock()
		l.markDirty("meta")
	}
}

// markDirty asks the saver to write a cache soon.
func (l *Library) markDirty(which string) {
	select {
	case l.dirty <- which:
	default:
	}
}

// saver writes the probe and metadata caches, at most every couple of seconds.
func (l *Library) saver() {
	pending := map[string]bool{}
	timer := time.NewTimer(time.Hour)
	for {
		select {
		case which := <-l.dirty:
			pending[which] = true
			timer.Reset(2 * time.Second)
		case <-timer.C:
			l.mu.RLock()
			if pending["probe"] {
				_ = writeJSON(filepath.Join(l.app.cacheDir, "probe.json"), l.probes)
			}
			if pending["meta"] {
				_ = writeJSON(filepath.Join(l.app.cacheDir, "meta.json"), l.metas)
			}
			l.mu.RUnlock()
			pending = map[string]bool{}
		}
	}
}

// ForgetMeta drops unmatched catalogue results so they are looked up again.
func (l *Library) ForgetMeta() {
	l.mu.Lock()
	for id, m := range l.metas {
		if m.Status != "ok" {
			delete(l.metas, id)
		}
	}
	l.mu.Unlock()
}

// Entry finds a file by id.
func (l *Library) Entry(id string) *Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, rt := range l.sources {
		if e := rt.entries[id]; e != nil {
			return e
		}
	}
	return nil
}

// Media returns the probed details of a file, or nil.
func (l *Library) Media(e *Entry) *Media {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if rec, ok := l.probes[e.ID]; ok && rec.Sig == probeSig(e) {
		return rec.Media
	}
	return nil
}

// backend returns the backend that holds a file.
func (l *Library) backend(e *Entry) Backend {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if rt := l.sources[e.SourceID]; rt != nil {
		return rt.backend
	}
	return nil
}

// Open opens a file for streaming for as long as ctx lasts.
func (l *Library) Open(ctx context.Context, e *Entry) (io.ReadSeekCloser, error) {
	b := l.backend(e)
	if b == nil {
		return nil, fmt.Errorf("that source was removed")
	}
	return b.Open(ctx, e.Rel)
}

// Input is what ffmpeg reads a file from: its local path, or Kinotape's own stream for share files.
func (l *Library) Input(e *Entry) string {
	if b := l.backend(e); b != nil {
		if local := b.LocalPath(e.Rel); local != "" {
			return local
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d/video/%s", l.app.port, e.ID)
}

// ExternalPath is what the operating system can open a file with.
func (l *Library) ExternalPath(e *Entry) string {
	if b := l.backend(e); b != nil {
		return b.ExternalPath(e.Rel)
	}
	return ""
}

// EpisodeView is one file as the UI sees it.
type EpisodeView struct {
	ID          string   `json:"id"`
	ShowID      string   `json:"show_id"`
	File        string   `json:"file"`
	Size        int64    `json:"size"`
	Season      *int     `json:"season"`
	Episode     *int     `json:"episode"`
	Title       *string  `json:"title"`
	Duration    *float64 `json:"duration"`
	Position    float64  `json:"position"`
	LastPlayed  *float64 `json:"last_played"`
	ResumeAt    float64  `json:"resume_at"`
	AutoWatched bool     `json:"auto_watched"`
	Watched     bool     `json:"watched"`
	Marked      bool     `json:"marked"`
	InApp       bool     `json:"in_app"`
	AudioLang   *string  `json:"audio_lang"`
	AudioCodec  string   `json:"audio_codec"`
	Subtitles   []Stream `json:"subtitles"`
	Fonts       []string `json:"fonts"`
	Source      string   `json:"source"`
	probed      bool
}

// ShowView is a show or movie with its files, progress and metadata.
type ShowView struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Kind         string         `json:"kind"`
	Year         *int           `json:"year"`
	Episodes     []*EpisodeView `json:"episodes"`
	NextID       string         `json:"next_id"`
	Mode         string         `json:"mode"`
	AfterID      *string        `json:"after_id"`
	LastPlayed   *float64       `json:"last_played"`
	WatchedCount int            `json:"watched_count"`
	Art          *string        `json:"art"`
	Genres       []string       `json:"genres"`
	Overview     string         `json:"overview"`
	Sources      []string       `json:"sources"`
}

// SourceView is a source's status for the settings page.
type SourceView struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Detail   string `json:"detail"`
	Count    int    `json:"count"`
	Scanning bool   `json:"scanning"`
	Error    string `json:"error"`
}

// LibraryView is everything the UI draws.
type LibraryView struct {
	Sources  []SourceView `json:"sources"`
	Autoplay bool         `json:"autoplay"`
	Artwork  bool         `json:"artwork"`
	TMDB     bool         `json:"tmdb"`
	FFmpeg   bool         `json:"ffmpeg"`
	Platform string       `json:"platform"`
	Updated  float64      `json:"updated"`
	Sig      string       `json:"sig"`
	Shows    []*ShowView  `json:"shows"`
	Genres   []string     `json:"genres"`
}

// View merges files, progress, marks and metadata into shows, and picks each show's next episode.
func (l *Library) View(cfg Config, st State, platform string) LibraryView {
	l.mu.RLock()
	shows := map[string]*ShowView{}
	var sources []SourceView
	for _, sid := range l.order {
		rt := l.sources[sid]
		sources = append(sources, SourceView{ID: sid, Kind: rt.src.Kind, Name: sourceName(rt.src), Detail: sourceDetail(rt.src),
			Count: len(rt.entries), Scanning: rt.scanning, Error: rt.err})
		for _, e := range rt.entries {
			key := e.Parsed.Show
			if e.Parsed.Kind == "movie" && e.Parsed.Year > 0 {
				key += " " + strconv.Itoa(e.Parsed.Year)
			}
			id := showID(key)
			show := shows[id]
			if show == nil {
				show = &ShowView{ID: id, Title: e.Parsed.Show, Kind: e.Parsed.Kind, Genres: []string{}}
				if e.Parsed.Year > 0 {
					year := e.Parsed.Year
					show.Year = &year
				}
				shows[id] = show
			}
			show.Episodes = append(show.Episodes, l.episodeView(e, id, sourceName(rt.src), st))
			if !contains(show.Sources, sourceName(rt.src)) {
				show.Sources = append(show.Sources, sourceName(rt.src))
			}
		}
	}
	var queue []metaJob
	genreSet := map[string]bool{}
	ordered := make([]*ShowView, 0, len(shows))
	for _, show := range shows {
		finishShow(show)
		meta, known := l.metas[show.ID]
		if meta.Status == "ok" {
			show.Genres = append(show.Genres, meta.Genres...)
			show.Overview = meta.Overview
			if show.Year == nil && meta.Year > 0 {
				year := meta.Year
				show.Year = &year
			}
			if meta.Poster {
				art := fmt.Sprintf("/art/%s?v=%d", show.ID, int64(meta.At))
				show.Art = &art
			}
		}
		for _, g := range show.Genres {
			genreSet[g] = true
		}
		if cfg.Artwork && !l.metaPending[show.ID] && metaStale(meta, known) && (l.app.ffprobe == "" || probed(show)) {
			year := 0
			if show.Year != nil {
				year = *show.Year
			}
			queue = append(queue, metaJob{id: show.ID, title: show.Title, kind: show.Kind, year: year, anime: looksLikeAnime(show)})
		}
		ordered = append(ordered, show)
	}
	l.mu.RUnlock()

	if len(queue) > 0 {
		l.mu.Lock()
		for _, job := range queue {
			if l.metaPending[job.id] {
				continue
			}
			select {
			case l.metaJobs <- job:
				l.metaPending[job.id] = true
			default:
			}
		}
		l.mu.Unlock()
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if la, lb := deref(a.LastPlayed), deref(b.LastPlayed); la != lb {
			return la > lb
		}
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	})
	genres := make([]string, 0, len(genreSet))
	for g := range genreSet {
		genres = append(genres, g)
	}
	sort.Strings(genres)
	view := LibraryView{Sources: sources, Autoplay: cfg.Autoplay, Artwork: cfg.Artwork, TMDB: cfg.TMDBKey != "",
		FFmpeg: l.app.ffmpeg != "" && l.app.ffprobe != "", Platform: platform, Shows: ordered, Genres: genres}
	if view.Sources == nil {
		view.Sources = []SourceView{}
	}
	sigData, _ := json.Marshal(view)
	view.Sig = md5Hex(string(sigData))
	view.Updated = now()
	return view
}

// episodeView builds the UI view of one file.
func (l *Library) episodeView(e *Entry, showID, source string, st State) *EpisodeView {
	var media *Media
	rec, probed := l.probes[e.ID]
	probed = probed && rec.Sig == probeSig(e)
	if probed {
		media = rec.Media
	}
	ep := &EpisodeView{ID: e.ID, ShowID: showID, File: path.Base(e.Rel), Size: e.Size, Source: source,
		Subtitles: []Stream{}, Fonts: []string{}, InApp: playsInBrowser(e.Rel, media), probed: probed}
	if e.Parsed.Kind == "series" {
		episode := e.Parsed.Episode
		ep.Episode = &episode
		if e.Parsed.Season > 0 {
			season := e.Parsed.Season
			ep.Season = &season
		}
		if e.Parsed.Title != "" {
			title := e.Parsed.Title
			ep.Title = &title
		}
	}
	if media != nil {
		if media.Duration > 0 {
			d := media.Duration
			ep.Duration = &d
		}
		ep.Subtitles, ep.Fonts = media.Subtitles, media.Fonts
		if len(media.Audio) > 0 {
			lang := media.Audio[0].Lang
			ep.AudioLang, ep.AudioCodec = &lang, media.Audio[0].Codec
		}
	}
	if p, ok := st.Progress[e.ID]; ok {
		ep.Position = p.Position
		at := p.At
		ep.LastPlayed = &at
	}
	ep.AutoWatched = finished(ep.Position, ep.Duration)
	mark, marked := st.Marks[e.ID]
	ep.Watched, ep.Marked = ep.AutoWatched, marked
	if marked {
		ep.Watched = mark
	}
	if !ep.Watched && !ep.AutoWatched && ep.Position >= resumeAfter {
		ep.ResumeAt = ep.Position
	}
	return ep
}

// probed reports whether ffprobe has looked at any of a show's files yet.
func probed(show *ShowView) bool {
	for _, e := range show.Episodes {
		if e.probed {
			return true
		}
	}
	return false
}

// looksLikeAnime guesses from Japanese audio or fansub-style names whether AniList should be asked first.
func looksLikeAnime(show *ShowView) bool {
	for _, e := range show.Episodes {
		if (e.AudioLang != nil && *e.AudioLang == "jpn") || strings.HasPrefix(e.File, "[") {
			return true
		}
	}
	return false
}

// finishShow sorts a show's episodes and works out what to offer next.
func finishShow(show *ShowView) {
	sort.Slice(show.Episodes, func(i, j int) bool {
		a, b := show.Episodes[i], show.Episodes[j]
		if sa, sb := derefInt(a.Season), derefInt(b.Season); sa != sb {
			return sa < sb
		}
		if ea, eb := derefInt(a.Episode), derefInt(b.Episode); ea != eb {
			return ea < eb
		}
		return a.File < b.File
	})
	next, mode := upNext(show.Episodes)
	show.NextID, show.Mode = next.ID, mode
	for i, e := range show.Episodes {
		if e.ID == next.ID && i+1 < len(show.Episodes) {
			after := show.Episodes[i+1].ID
			show.AfterID = &after
		}
		if e.Watched {
			show.WatchedCount++
		}
		if e.LastPlayed != nil && (show.LastPlayed == nil || *e.LastPlayed > *show.LastPlayed) {
			played := *e.LastPlayed
			show.LastPlayed = &played
		}
	}
}

// upNext picks what to offer, Netflix style: resume the last one played, else the first unwatched after it.
func upNext(eps []*EpisodeView) (*EpisodeView, string) {
	offer := func(e *EpisodeView) (*EpisodeView, string) {
		if e.ResumeAt > 0 {
			return e, "resume"
		}
		return e, "start"
	}
	var last *EpisodeView
	lastIndex := -1
	for i, e := range eps {
		if e.LastPlayed != nil && (last == nil || *e.LastPlayed > *last.LastPlayed) {
			last, lastIndex = e, i
		}
	}
	if last != nil {
		if !last.Watched {
			return offer(last)
		}
		for _, e := range eps[lastIndex+1:] {
			if !e.Watched {
				return offer(e)
			}
		}
	}
	for _, e := range eps {
		if !e.Watched {
			return offer(e)
		}
	}
	return eps[0], "rewatch"
}

// finished treats an episode as watched once only the end credits could be left.
func finished(position float64, duration *float64) bool {
	if duration == nil || *duration <= 0 || position <= 0 {
		return false
	}
	return *duration-position <= math.Max(120, *duration*0.08)
}

// metaStale reports whether a show should be looked up (again).
func metaStale(meta Meta, known bool) bool {
	if !known {
		return true
	}
	age := now() - meta.At
	switch meta.Status {
	case "ok":
		return age > 60*86400
	case "error":
		return age > 3600
	default:
		return age > 7*86400
	}
}

// sourceName is a short label for a source.
func sourceName(src Source) string {
	if src.Name != "" {
		return src.Name
	}
	if src.Kind == "smb" {
		label := src.Share
		if src.Dir != "" {
			label = path.Base(strings.ReplaceAll(src.Dir, `\`, "/"))
		}
		return label
	}
	return filepath.Base(src.Path)
}

// sourceDetail is where a source lives, for the settings page.
func sourceDetail(src Source) string {
	if src.Kind == "smb" {
		detail := `\\` + src.Host + `\` + src.Share
		if src.Dir != "" {
			detail += `\` + strings.ReplaceAll(strings.Trim(src.Dir, `/\`), "/", `\`)
		}
		return detail
	}
	return src.Path
}

// contains reports whether a list holds a string.
func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// deref reads an optional float, treating nil as zero.
func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// derefInt reads an optional int, treating nil as zero.
func derefInt(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}
