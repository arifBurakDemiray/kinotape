package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Meta is what an online catalogue knows about a show or movie.
type Meta struct {
	Status   string   `json:"status"`
	Provider string   `json:"provider,omitempty"`
	Title    string   `json:"title,omitempty"`
	Year     int      `json:"year,omitempty"`
	Genres   []string `json:"genres,omitempty"`
	Overview string   `json:"overview,omitempty"`
	Poster   bool     `json:"poster,omitempty"`
	At       float64  `json:"at"`
}

// candidate is one search hit from a catalogue.
type candidate struct {
	provider string
	names    []string
	year     int
	genres   []string
	overview string
	poster   string
}

var (
	tagRe          = regexp.MustCompile(`<[^>]*>`)
	parenRe        = regexp.MustCompile(`\s*\([^)]*\)`)
	wikiGenreRules = []struct{ word, genre string }{
		{"action", "Action"}, {"adventure", "Adventure"}, {"comedy", "Comedy"}, {"sitcom", "Comedy"}, {"drama", "Drama"},
		{"horror", "Horror"}, {"thriller", "Thriller"}, {"science fiction", "Sci-Fi"}, {"fantasy", "Fantasy"},
		{"romance", "Romance"}, {"romantic", "Romance"}, {"crime", "Crime"}, {"heist", "Crime"}, {"gangster", "Crime"},
		{"mystery", "Mystery"}, {"detective", "Mystery"}, {"anim", "Animation"}, {"documentary", "Documentary"},
		{"family", "Family"}, {"children", "Family"}, {"war film", "War"}, {"war drama", "War"}, {"western", "Western"},
		{"musical", "Musical"}, {"historical", "History"}, {"biographical", "Biography"}, {"sport", "Sport"},
		{"superhero", "Superhero"}, {"psychological", "Psychological"}, {"satir", "Comedy"},
	}
	httpClient     = &http.Client{Timeout: 15 * time.Second}
	tmdbGenreCache = map[string]map[int]string{}
	tmdbGenreMu    sync.Mutex
	wikimediaGate  sync.Mutex
	wikimediaNext  time.Time
)

const userAgent = "Kinotape/1.0 (local video library)"

// lookupFunc searches one catalogue.
type lookupFunc func(title string, year int, kind string) ([]candidate, error)

// catalogues orders the catalogues for a title: AniList first for anime, the TV and film catalogues first otherwise.
func catalogues(kind string, anime bool, tmdbKey string) []lookupFunc {
	var tmdb []lookupFunc
	if tmdbKey != "" {
		tmdb = []lookupFunc{func(t string, y int, k string) ([]candidate, error) { return lookupTMDB(t, y, k, tmdbKey) }}
	}
	general := []lookupFunc{lookupWikipedia}
	if kind == "series" {
		general = []lookupFunc{lookupTVmaze, lookupWikipedia}
	}
	if anime {
		return append(append([]lookupFunc{lookupAniList}, general...), tmdb...)
	}
	return append(append(tmdb, general...), lookupAniList)
}

// fetchMeta looks a title up in the catalogues in turn and saves the first match's poster to posterPath.
func fetchMeta(title string, year int, kind string, anime bool, tmdbKey string, posterPath string) Meta {
	lookups := catalogues(kind, anime, tmdbKey)
	failed := false
	for _, lookup := range lookups {
		found, err := lookup(title, year, kind)
		if err != nil {
			failed = true
			continue
		}
		if best := bestMatch(found, title, year); best != nil {
			meta := Meta{Status: "ok", Provider: best.provider, Title: best.names[0], Year: best.year,
				Genres: normalizeGenres(best.genres), Overview: best.overview, At: now()}
			if best.poster != "" && downloadFile(best.poster, posterPath) == nil {
				meta.Poster = true
			}
			return meta
		}
	}
	status := "none"
	if failed {
		status = "error"
	}
	return Meta{Status: status, At: now()}
}

// bestMatch picks the hit whose name matches the title exactly, preferring the closest year.
func bestMatch(found []candidate, title string, year int) *candidate {
	wanted := normalizeTitle(title)
	var best *candidate
	for i := range found {
		c := &found[i]
		matched := false
		for _, name := range c.names {
			if name != "" && normalizeTitle(name) == wanted {
				matched = true
				break
			}
		}
		if !matched || (year > 0 && c.year > 0 && abs(c.year-year) > 1) {
			continue
		}
		if best == nil {
			best = c
		}
	}
	return best
}

// lookupAniList searches AniList, which covers anime series and films.
func lookupAniList(title string, _ int, _ string) ([]candidate, error) {
	query := `query($q:String){Page(perPage:8){media(search:$q,type:ANIME){title{romaji english native} synonyms genres ` +
		`seasonYear startDate{year} description(asHtml:false) coverImage{extraLarge large}}}}`
	body, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]string{"q": title}})
	var out struct {
		Data struct {
			Page struct {
				Media []struct {
					Title      map[string]string `json:"title"`
					Synonyms   []string          `json:"synonyms"`
					Genres     []string          `json:"genres"`
					SeasonYear int               `json:"seasonYear"`
					StartDate  struct {
						Year int `json:"year"`
					} `json:"startDate"`
					Description string            `json:"description"`
					CoverImage  map[string]string `json:"coverImage"`
				} `json:"media"`
			} `json:"Page"`
		} `json:"data"`
	}
	if err := requestJSON("POST", "https://graphql.anilist.co", body, nil, &out); err != nil {
		return nil, err
	}
	var found []candidate
	for _, m := range out.Data.Page.Media {
		names := []string{m.Title["english"], m.Title["romaji"], m.Title["native"]}
		names = append(names, m.Synonyms...)
		if names[0] == "" {
			names[0] = m.Title["romaji"]
		}
		poster := m.CoverImage["extraLarge"]
		if poster == "" {
			poster = m.CoverImage["large"]
		}
		year := m.StartDate.Year
		if year == 0 {
			year = m.SeasonYear
		}
		found = append(found, candidate{provider: "anilist", names: names, year: year, genres: append([]string{"Anime"}, m.Genres...),
			overview: cleanText(m.Description), poster: poster})
	}
	return found, nil
}

// lookupTVmaze searches TVmaze, which covers TV series and needs no key.
func lookupTVmaze(title string, _ int, _ string) ([]candidate, error) {
	var out []struct {
		Show struct {
			Name      string   `json:"name"`
			Genres    []string `json:"genres"`
			Premiered string   `json:"premiered"`
			Summary   string   `json:"summary"`
			Image     *struct {
				Original string `json:"original"`
				Medium   string `json:"medium"`
			} `json:"image"`
		} `json:"show"`
	}
	if err := requestJSON("GET", "https://api.tvmaze.com/search/shows?q="+url.QueryEscape(title), nil, nil, &out); err != nil {
		return nil, err
	}
	var found []candidate
	for _, hit := range out {
		year := 0
		if len(hit.Show.Premiered) >= 4 {
			year, _ = strconv.Atoi(hit.Show.Premiered[:4])
		}
		poster := ""
		if hit.Show.Image != nil {
			poster = hit.Show.Image.Original
		}
		found = append(found, candidate{provider: "tvmaze", names: []string{hit.Show.Name}, year: year, genres: hit.Show.Genres,
			overview: cleanText(hit.Show.Summary), poster: poster})
	}
	return found, nil
}

// lookupWikipedia finds a film's or series' Wikipedia article, taking the lead image as the poster and genres from Wikidata.
func lookupWikipedia(title string, year int, kind string) ([]candidate, error) {
	query := title + " film"
	if year > 0 && kind == "movie" {
		query = fmt.Sprintf("%s %d film", title, year)
	} else if kind == "series" {
		query = title + " television series"
	}
	params := url.Values{"action": {"query"}, "list": {"search"}, "srsearch": {query}, "srlimit": {"6"}, "format": {"json"}}
	var search struct {
		Query struct {
			Search []struct {
				Title string `json:"title"`
			} `json:"search"`
		} `json:"query"`
	}
	if err := wikimediaJSON("https://en.wikipedia.org/w/api.php?"+params.Encode(), &search); err != nil {
		return nil, err
	}
	for _, hit := range search.Query.Search {
		base := strings.TrimSpace(parenRe.ReplaceAllString(hit.Title, ""))
		if normalizeTitle(base) != normalizeTitle(title) {
			continue
		}
		var page struct {
			Description string `json:"description"`
			Extract     string `json:"extract"`
			Item        string `json:"wikibase_item"`
			Thumbnail   struct {
				Source string `json:"source"`
			} `json:"thumbnail"`
			Original struct {
				Source string `json:"source"`
			} `json:"originalimage"`
		}
		summary := "https://en.wikipedia.org/api/rest_v1/page/summary/" + url.PathEscape(strings.ReplaceAll(hit.Title, " ", "_"))
		if err := wikimediaJSON(summary, &page); err != nil {
			return nil, err
		}
		desc := strings.ToLower(page.Description)
		isFilm := strings.Contains(desc, "film") || strings.Contains(desc, "movie")
		isShow := strings.Contains(desc, "series") || strings.Contains(desc, "television") || strings.Contains(desc, "sitcom")
		if (kind == "movie" && !isFilm) || (kind == "series" && !isShow) {
			continue
		}
		found := 0
		if m := yearRe.FindStringSubmatch(page.Description); m != nil {
			found, _ = strconv.Atoi(m[1])
		}
		poster := page.Thumbnail.Source
		if poster == "" {
			poster = page.Original.Source
		}
		genres, err := wikidataGenres(page.Item)
		if err != nil {
			return nil, err
		}
		return []candidate{{provider: "wikipedia", names: []string{base}, year: found, genres: genres,
			overview: cleanText(page.Extract), poster: poster}}, nil
	}
	return nil, nil
}

// wikidataGenres reads a Wikidata item's genre claims and maps their labels onto Kinotape's genre names.
func wikidataGenres(item string) ([]string, error) {
	if item == "" {
		return nil, nil
	}
	var claims struct {
		Claims map[string][]struct {
			Mainsnak struct {
				Datavalue struct {
					Value struct {
						ID string `json:"id"`
					} `json:"value"`
				} `json:"datavalue"`
			} `json:"mainsnak"`
		} `json:"claims"`
	}
	params := url.Values{"action": {"wbgetclaims"}, "entity": {item}, "property": {"P136"}, "format": {"json"}}
	if err := wikimediaJSON("https://www.wikidata.org/w/api.php?"+params.Encode(), &claims); err != nil {
		return nil, err
	}
	var ids []string
	for _, claim := range claims.Claims["P136"] {
		if id := claim.Mainsnak.Datavalue.Value.ID; id != "" && len(ids) < 40 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var labels struct {
		Entities map[string]struct {
			Labels map[string]struct {
				Value string `json:"value"`
			} `json:"labels"`
		} `json:"entities"`
	}
	params = url.Values{"action": {"wbgetentities"}, "ids": {strings.Join(ids, "|")}, "props": {"labels"}, "languages": {"en"}, "format": {"json"}}
	if err := wikimediaJSON("https://www.wikidata.org/w/api.php?"+params.Encode(), &labels); err != nil {
		return nil, err
	}
	var genres []string
	for _, id := range ids {
		label := strings.ToLower(labels.Entities[id].Labels["en"].Value)
		for _, rule := range wikiGenreRules {
			if strings.Contains(label, rule.word) && !contains(genres, rule.genre) {
				genres = append(genres, rule.genre)
			}
		}
	}
	return genres, nil
}

// wikimediaJSON calls a Wikipedia or Wikidata API at a polite pace, which their public rate limits ask for.
func wikimediaJSON(target string, out any) error {
	wikimediaGate.Lock()
	if wait := time.Until(wikimediaNext); wait > 0 {
		time.Sleep(wait)
	}
	wikimediaNext = time.Now().Add(1200 * time.Millisecond)
	wikimediaGate.Unlock()
	return requestJSON("GET", target, nil, nil, out)
}

// lookupTMDB searches The Movie Database with the user's own API key or read token.
func lookupTMDB(title string, year int, kind string, key string) ([]candidate, error) {
	media := "movie"
	if kind == "series" {
		media = "tv"
	}
	params := url.Values{"query": {title}}
	if year > 0 {
		params.Set(map[string]string{"movie": "year", "tv": "first_air_date_year"}[media], strconv.Itoa(year))
	}
	var out struct {
		Results []struct {
			Title        string `json:"title"`
			Name         string `json:"name"`
			OriginalName string `json:"original_name"`
			Original     string `json:"original_title"`
			Release      string `json:"release_date"`
			FirstAir     string `json:"first_air_date"`
			Overview     string `json:"overview"`
			Poster       string `json:"poster_path"`
			GenreIDs     []int  `json:"genre_ids"`
		} `json:"results"`
	}
	if err := tmdbGet("/search/"+media, params, key, &out); err != nil {
		return nil, err
	}
	names, _ := tmdbGenres(media, key)
	var found []candidate
	for _, r := range out.Results {
		date := r.Release + r.FirstAir
		y := 0
		if len(date) >= 4 {
			y, _ = strconv.Atoi(date[:4])
		}
		var genres []string
		for _, id := range r.GenreIDs {
			if name := names[id]; name != "" {
				genres = append(genres, name)
			}
		}
		poster := ""
		if r.Poster != "" {
			poster = "https://image.tmdb.org/t/p/w500" + r.Poster
		}
		found = append(found, candidate{provider: "tmdb", names: []string{r.Title + r.Name, r.Original + r.OriginalName}, year: y,
			genres: genres, overview: r.Overview, poster: poster})
	}
	return found, nil
}

// tmdbGenres returns TMDB's genre names by id, cached per media type.
func tmdbGenres(media, key string) (map[int]string, error) {
	tmdbGenreMu.Lock()
	defer tmdbGenreMu.Unlock()
	if cached := tmdbGenreCache[media]; cached != nil {
		return cached, nil
	}
	var out struct {
		Genres []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"genres"`
	}
	if err := tmdbGet("/genre/"+media+"/list", url.Values{}, key, &out); err != nil {
		return map[int]string{}, err
	}
	names := map[int]string{}
	for _, g := range out.Genres {
		names[g.ID] = g.Name
	}
	tmdbGenreCache[media] = names
	return names, nil
}

// tmdbGet calls the TMDB v3 API; long keys are v4 read tokens and go in the Authorization header.
func tmdbGet(endpoint string, params url.Values, key string, out any) error {
	headers := map[string]string{}
	if len(key) > 40 {
		headers["Authorization"] = "Bearer " + key
	} else {
		params.Set("api_key", key)
	}
	return requestJSON("GET", "https://api.themoviedb.org/3"+endpoint+"?"+params.Encode(), nil, headers, out)
}

// requestJSON sends a request and decodes a JSON answer.
func requestJSON(method, target string, body []byte, headers map[string]string, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", req.URL.Host, res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out)
}

// downloadFile saves a URL to a file.
func downloadFile(target, dest string) error {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("poster download failed")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// normalizeGenres merges the different catalogues' genre names into one vocabulary.
func normalizeGenres(genres []string) []string {
	rename := map[string]string{"Science-Fiction": "Sci-Fi", "Science Fiction": "Sci-Fi", "Mahou Shoujo": "Magical Girl",
		"Kids": "Family", "Children": "Family"}
	seen := map[string]bool{}
	var out []string
	for _, g := range genres {
		for _, part := range strings.Split(g, " & ") {
			part = strings.TrimSpace(part)
			if r, ok := rename[part]; ok {
				part = r
			}
			if part != "" && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out
}

// cleanText strips HTML tags and entities from a synopsis.
func cleanText(text string) string {
	text = tagRe.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	text = strings.TrimSpace(strings.Split(text, "(Source:")[0])
	return strings.Join(strings.Fields(text), " ")
}

// now is the current time in Unix seconds.
func now() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// abs is the absolute value of an int.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
