package main

import (
	"crypto/md5"
	"encoding/hex"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Parsed is what a video's file name says about it.
type Parsed struct {
	Kind       string
	Show       string
	Season     int
	Episode    int
	HasEpisode bool
	Title      string
	Year       int
}

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".mov": true, ".avi": true, ".webm": true,
	".wmv": true, ".flv": true, ".mpg": true, ".mpeg": true, ".m2ts": true,
}

var (
	qualityRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(\d{3,4}p|4k|uhd|hdr(?:10)?\+?|dv|sdr|web[ ._-]?(?:dl|rip)|` +
		`blu[ ._-]?ray|bd(?:rip|remux)?|brrip|hdtv|dvd(?:rip)?|remux|x26[45]|h[ ._]?26[45]|` +
		`hevc|avc|xvid|aac(?:[ ._]?\d[ ._]\d)?|e?ac3|ddp?(?:[ ._]?\d[ ._]\d)?|dts|flac|opus|truehd|atmos|` +
		`dual[ ._-]?audio|multi(?:subs?)?|msubs|10[ ._-]?bit|nf|amzn|cr|hmax|dsnp|atvp|proper|repack)(?:[^a-z0-9]|$)`)
	yearRe       = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})(?:[^0-9]|$)`)
	sxxeyyRe     = regexp.MustCompile(`(?i)^(?P<show>.*?)[\s._-]*\bS(?P<season>\d{1,2})[\s._-]?E(?P<episode>\d{1,4})(?:[\s._-]?E\d{1,4})*(?P<rest>.*)$`)
	nxnnRe       = regexp.MustCompile(`(?i)^(?P<show>.*?)[\s._-]+(?P<season>\d{1,2})x(?P<episode>\d{2,3})(?P<rest>[^0-9].*)?$`)
	dashRe       = regexp.MustCompile(`(?i)^(?P<show>.+?)\s+-\s+(?:(?:ep|episode)[\s.]*)?(?P<episode>\d{1,4})(?:v\d+)?(?P<rest>[^0-9].*)?$`)
	epWordRe     = regexp.MustCompile(`(?i)^(?P<show>.+?)[\s._-]+(?:ep|episode)[\s._-]*(?P<episode>\d{1,4})(?P<rest>[^0-9].*)?$`)
	seasonDirRe  = regexp.MustCompile(`(?i)^(?:season|series|s)[\s._-]*(\d{1,2})$`)
	bracketsRe   = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)`)
	leadingTagRe = regexp.MustCompile(`^(?:\s*\[[^\]]*\])+\s*`)
	splitYearRe  = regexp.MustCompile(`^(.*?)[\s._(-]+((?:19|20)\d{2})[)\s._-]*$`)
	groupOnlyRe  = regexp.MustCompile(`^\s*-[A-Za-z0-9]+\s*$`)
	sampleRe     = regexp.MustCompile(`(?i)(?:^|[^a-z])sample(?:[^a-z]|$)`)
	separatorRe  = regexp.MustCompile(`[._\[\]()]+`)
	spacesRe     = regexp.MustCompile(`\s+`)
	nonAlnumRe   = regexp.MustCompile(`[^a-z0-9]+`)
	episodeRes   = []*regexp.Regexp{sxxeyyRe, nxnnRe, dashRe, epWordRe}
)

// isVideo reports whether a file name has a video extension.
func isVideo(name string) bool {
	return videoExts[strings.ToLower(path.Ext(name))]
}

// isSample reports whether a file looks like a release's short sample clip.
func isSample(name string, size int64) bool {
	return sampleRe.MatchString(name) && size < 300<<20
}

// tidy turns a dotted release-name fragment into readable words.
func tidy(text string) string {
	text = bracketsRe.ReplaceAllString(text, " ")
	text = separatorRe.ReplaceAllString(text, " ")
	text = spacesRe.ReplaceAllString(text, " ")
	return strings.Trim(text, " -_")
}

// beforeQuality cuts a release-name fragment at its first quality, codec or source tag.
func beforeQuality(text string) string {
	if loc := qualityRe.FindStringSubmatchIndex(text); loc != nil {
		return text[:loc[2]]
	}
	return text
}

// splitYear separates a trailing release year from a title, keeping titles that are only a year.
func splitYear(title string) (string, int) {
	m := splitYearRe.FindStringSubmatch(title)
	if m != nil && strings.Trim(m[1], " ._-") != "" {
		year, _ := strconv.Atoi(m[2])
		return m[1], year
	}
	return title, 0
}

// showFromFolders falls back to folder names when a file name carries no show title.
func showFromFolders(rel string, season int) (string, int) {
	parts := strings.Split(path.Dir(rel), "/")
	if len(parts) == 0 || parts[len(parts)-1] == "." {
		return "", season
	}
	name := parts[len(parts)-1]
	if m := seasonDirRe.FindStringSubmatch(name); m != nil {
		if season == 0 {
			season, _ = strconv.Atoi(m[1])
		}
		if len(parts) < 2 {
			return "", season
		}
		name = parts[len(parts)-2]
	}
	title, _ := splitYear(beforeQuality(name))
	if t := tidy(title); t != "" {
		return t, season
	}
	return name, season
}

// parseVideo works out show, season, episode number and episode title from a video's path.
func parseVideo(rel string) Parsed {
	base := path.Base(rel)
	stem := norm.NFC.String(strings.TrimSuffix(base, path.Ext(base)))
	bare := leadingTagRe.ReplaceAllString(stem, "")
	for _, re := range episodeRes {
		m := re.FindStringSubmatch(bare)
		if m == nil {
			continue
		}
		group := func(name string) string {
			if i := re.SubexpIndex(name); i >= 0 {
				return m[i]
			}
			return ""
		}
		show, _ := splitYear(beforeQuality(group("show")))
		show = tidy(show)
		season, _ := strconv.Atoi(group("season"))
		if show == "" {
			show, season = showFromFolders(rel, season)
		}
		if show == "" {
			show = tidy(stem)
		}
		episode, _ := strconv.Atoi(group("episode"))
		rest := group("rest")
		title := ""
		if !groupOnlyRe.MatchString(rest) {
			title = tidy(beforeQuality(rest))
		}
		return Parsed{Kind: "series", Show: show, Season: season, Episode: episode, HasEpisode: true, Title: title}
	}
	head := beforeQuality(bare)
	year := 0
	if loc := yearRe.FindStringSubmatchIndex(head); loc != nil && loc[2] > 0 {
		year, _ = strconv.Atoi(head[loc[2]:loc[3]])
		head = head[:loc[2]]
	}
	title := tidy(head)
	if title == "" {
		title = tidy(stem)
	}
	if title == "" {
		title = stem
	}
	return Parsed{Kind: "movie", Show: title, Year: year}
}

// fold lower-cases text and strips accents so titles compare loosely.
func fold(text string) string {
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	folded, _, err := transform.String(t, text)
	if err != nil {
		folded = text
	}
	return strings.ToLower(folded)
}

// normalizeTitle reduces a title to lower-case letters and digits for matching.
func normalizeTitle(text string) string {
	return nonAlnumRe.ReplaceAllString(fold(text), "")
}

// showID is a stable, URL-safe id for a show title.
func showID(title string) string {
	slug := strings.Trim(nonAlnumRe.ReplaceAllString(fold(title), "-"), "-")
	if slug != "" {
		return slug
	}
	return md5Hex(title)[:12]
}

// md5Hex returns the hex md5 of a string.
func md5Hex(text string) string {
	sum := md5.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}
