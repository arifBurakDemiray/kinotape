package main

import "testing"

// TestParseVideo checks the release-name shapes Kinotape has to understand.
func TestParseVideo(t *testing.T) {
	cases := []struct {
		rel     string
		kind    string
		show    string
		season  int
		episode int
		title   string
		year    int
	}{
		{"Lights/Northern.Lights.S01E07.The.Long.Night.1080p.NF.WEB-DL.JPN.AAC2.0.H.264.MSubs-GROUP.mkv", "series", "Northern Lights", 1, 7, "The Long Night", 0},
		{"Lights/Northern.Lights.S01E10.Salt-Water.and.Sea-Glass.1080p.NF.WEB-DL.JPN.AAC2.0.H.264.MSubs-GROUP.mkv", "series", "Northern Lights", 1, 10, "Salt-Water and Sea-Glass", 0},
		{"Lights/Northern.Lights.S01E16.Paper.and.Stone.1080p.CR.WEB-DL.MULTi.AAC2.0.H.264.MSubs-GROUP.mkv", "series", "Northern Lights", 1, 16, "Paper and Stone", 0},
		{"Harbor/[FanSub][Team][Encoder] Harbor - 26.mkv", "series", "Harbor", 0, 26, "", 0},
		{"x/[SomeSubs] Blue Harbor - 07 (1080p) [ABCD1234].mkv", "series", "Blue Harbor", 0, 7, "", 0},
		{"x/Show Name - S02E03 - The Title.mkv", "series", "Show Name", 2, 3, "The Title", 0},
		{"x/Show.2024.S01E01-GROUP.mkv", "series", "Show", 1, 1, "", 0},
		{"Breaking Bad/Season 2/S02E05.mkv", "series", "Breaking Bad", 2, 5, "", 0},
		{"x/The.Office.3x07.Branch.Wars.720p.mkv", "series", "The Office", 3, 7, "Branch Wars", 0},
		{"x/One Piece Episode 1071.mp4", "series", "One Piece", 0, 1071, "", 0},
		{"Quiet.Signal.2017.DUAL.1080p.BluRay.CRF.x264-GROUP/Quiet.Signal.2017.DUAL.1080p.BluRay.CRF.x264-GROUP.mkv", "movie", "Quiet Signal", 0, 0, "", 2017},
		{"Glass.Garden.The.Movie.-.Winter.Arc.2025.2160p.WEB-DL.DUAL.DDP5.1.DV.HDR10+.H.265-GROUP.mkv", "movie", "Glass Garden The Movie - Winter Arc", 0, 0, "", 2025},
	}
	for _, c := range cases {
		got := parseVideo(c.rel)
		if got.Kind != c.kind || got.Show != c.show || got.Season != c.season || got.Episode != c.episode || got.Title != c.title || got.Year != c.year {
			t.Errorf("%s\n got  %+v\n want kind=%s show=%q season=%d episode=%d title=%q year=%d", c.rel, got, c.kind, c.show, c.season, c.episode, c.title, c.year)
		}
	}
}

// TestShowID keeps ids stable across punctuation and case.
func TestShowID(t *testing.T) {
	if a, b := showID("The Northern Lights of Home"), showID("the northern lights of home"); a != b || a != "the-northern-lights-of-home" {
		t.Errorf("ids differ: %q %q", a, b)
	}
}
