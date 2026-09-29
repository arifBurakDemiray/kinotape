package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stream describes one audio or subtitle stream inside a video file.
type Stream struct {
	Index   int    `json:"index"`
	Codec   string `json:"codec"`
	Lang    string `json:"lang"`
	Title   string `json:"title"`
	Default bool   `json:"default"`
	Forced  bool   `json:"forced"`
}

// Media is what ffprobe found inside a file.
type Media struct {
	Duration   float64  `json:"duration"`
	VideoCodec string   `json:"video_codec"`
	PixFmt     string   `json:"pix_fmt"`
	Audio      []Stream `json:"audio"`
	Subtitles  []Stream `json:"subtitles"`
	Fonts      []string `json:"fonts"`
}

const probeVersion = 3

var (
	browserContainers = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".mov": true, ".webm": true}
	browserVideo      = map[string]bool{"h264": true, "hevc": true, "vp8": true, "vp9": true, "av1": true}
	browserAudio      = map[string]bool{"aac": true, "mp3": true, "opus": true, "vorbis": true, "flac": true}
	textSubtitles     = map[string]bool{"subrip": true, "ass": true, "ssa": true, "webvtt": true, "mov_text": true, "text": true}
	fontExts          = map[string]bool{".ttf": true, ".otf": true, ".ttc": true, ".woff": true, ".woff2": true}
	defaultStyleRe    = regexp.MustCompile(`(?m)^Style: Default,.*$`)
)

// srtStyle replaces ffmpeg's small Arial default when plain-text subtitles are turned into ASS.
const srtStyle = "Style: Default,Liberation Sans,19,&H00FFFFFF,&H000000FF,&H00000000,&H00000000," +
	"-1,0,0,0,100,100,0,0,1,1,0,2,16,16,24,1"

// findTool locates ffmpeg or ffprobe next to Kinotape, on the PATH, or in the usual install folders.
func findTool(name string) string {
	exe := name
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	var dirs []string
	if self, err := os.Executable(); err == nil {
		dir := filepath.Dir(self)
		dirs = append(dirs, dir, filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "..", "Resources"))
	}
	for _, dir := range dirs {
		if candidate := filepath.Join(dir, exe); fileExists(candidate) {
			return candidate
		}
	}
	if found, err := exec.LookPath(exe); err == nil {
		return found
	}
	extra := []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/snap/bin"}
	if runtime.GOOS == "windows" {
		extra = []string{
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WinGet", "Links"),
			filepath.Join(os.Getenv("ProgramFiles"), "ffmpeg", "bin"),
			`C:\ffmpeg\bin`,
			filepath.Join(os.Getenv("USERPROFILE"), "scoop", "shims"),
			`C:\ProgramData\chocolatey\bin`,
		}
	}
	for _, dir := range extra {
		if candidate := filepath.Join(dir, exe); fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

// fileExists reports whether a regular file exists at p.
func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// runTool runs ffmpeg or ffprobe without a console window and returns its output.
func runTool(ctx context.Context, dir string, tool string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Dir = dir
	hideWindow(cmd)
	return cmd.Output()
}

// probeMedia reads a file's duration and streams with ffprobe.
func probeMedia(ffprobe, input string) (*Media, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := runTool(ctx, "", ffprobe, "-v", "error", "-show_entries",
		"format=duration:stream=index,codec_type,codec_name,pix_fmt:stream_disposition=default,forced,attached_pic"+
			":stream_tags=language,title,filename,mimetype",
		"-of", "json", input)
	if err != nil {
		return nil, err
	}
	var data struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Index       int               `json:"index"`
			CodecType   string            `json:"codec_type"`
			CodecName   string            `json:"codec_name"`
			PixFmt      string            `json:"pix_fmt"`
			Disposition map[string]int    `json:"disposition"`
			Tags        map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, err
	}
	m := &Media{Audio: []Stream{}, Subtitles: []Stream{}, Fonts: []string{}}
	m.Duration, _ = strconv.ParseFloat(data.Format.Duration, 64)
	for _, s := range data.Streams {
		stream := Stream{Index: s.Index, Codec: s.CodecName, Lang: s.Tags["language"], Title: s.Tags["title"],
			Default: s.Disposition["default"] == 1, Forced: s.Disposition["forced"] == 1}
		switch s.CodecType {
		case "video":
			if s.Disposition["attached_pic"] == 0 && m.VideoCodec == "" {
				m.VideoCodec, m.PixFmt = s.CodecName, s.PixFmt
			}
		case "audio":
			m.Audio = append(m.Audio, stream)
		case "subtitle":
			if textSubtitles[s.CodecName] {
				m.Subtitles = append(m.Subtitles, stream)
			}
		case "attachment":
			name := s.Tags["filename"]
			if name != "" && !strings.ContainsAny(name, `/\`) && fontExts[strings.ToLower(path.Ext(name))] {
				m.Fonts = append(m.Fonts, name)
			}
		}
	}
	return m, nil
}

// playsInBrowser reports whether Chrome or Edge can decode the container, picture and first audio track.
func playsInBrowser(name string, m *Media) bool {
	if !browserContainers[strings.ToLower(path.Ext(name))] {
		return false
	}
	if m == nil {
		return true
	}
	if m.VideoCodec == "" || !browserVideo[m.VideoCodec] {
		return false
	}
	// Chrome has no decoder for 10-bit H.264, a common anime encode.
	if m.VideoCodec == "h264" && strings.Contains(m.PixFmt, "10") {
		return false
	}
	if len(m.Audio) == 0 {
		return true
	}
	codec := m.Audio[0].Codec
	return browserAudio[codec] || strings.HasPrefix(codec, "pcm_")
}

// keyedLocks hands out one mutex per cache file so concurrent requests extract it once.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// get returns the mutex for a key.
func (k *keyedLocks) get(key string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.locks == nil {
		k.locks = map[string]*sync.Mutex{}
	}
	if k.locks[key] == nil {
		k.locks[key] = &sync.Mutex{}
	}
	return k.locks[key]
}

// makeThumbnail grabs one still from the video with ffmpeg.
func makeThumbnail(ffmpeg, input, out string, duration float64) error {
	at := duration * 0.3
	if duration >= 600 {
		at = duration * 0.15
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tmp := out + ".part.jpg"
	_, err := runTool(ctx, "", ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-ss", fmt.Sprintf("%.2f", at),
		"-i", input, "-frames:v", "1", "-vf", "scale=960:-2", "-q:v", "4", "-y", tmp)
	return finishFile(tmp, out, err)
}

// extractSubtitle converts one subtitle stream to WebVTT or ASS; plain-text streams get Kinotape's ASS style.
func extractSubtitle(ffmpeg, input, out string, stream Stream, format string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	codec := "ass"
	if format == "vtt" {
		codec = "webvtt"
	}
	tmp := out + ".part"
	_, err := runTool(ctx, "", ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", input,
		"-map", fmt.Sprintf("0:%d", stream.Index), "-c:s", codec, "-f", codec, "-y", tmp)
	if err == nil && format == "ass" && stream.Codec != "ass" && stream.Codec != "ssa" {
		if data, readErr := os.ReadFile(tmp); readErr == nil {
			err = os.WriteFile(tmp, []byte(defaultStyleRe.ReplaceAllLiteralString(string(data), srtStyle)), 0o644)
		}
	}
	return finishFile(tmp, out, err)
}

// extractFonts writes every font attached to a video into dir.
func extractFonts(ffmpeg, input, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// ffmpeg exits with an error after dumping because there is no real output; the fonts are still written.
	_, _ = runTool(ctx, dir, ffmpeg, "-nostdin", "-loglevel", "error", "-y", "-dump_attachment:t", "", "-i", input,
		"-t", "0", "-f", "null", "-")
	return os.WriteFile(filepath.Join(dir, ".done"), nil, 0o644)
}

// finishFile moves a finished temp file into place, or removes it when the tool failed.
func finishFile(tmp, out string, err error) error {
	if info, statErr := os.Stat(tmp); err == nil && statErr == nil && info.Size() > 0 {
		return os.Rename(tmp, out)
	}
	_ = os.Remove(tmp)
	if err == nil {
		err = fmt.Errorf("ffmpeg produced no output")
	}
	return err
}
