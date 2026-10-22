// Package naming turns paths into movies, series, seasons and episodes.
//
// Pure functions only, no filesystem access, so everything here is unit-testable.
// The rules follow the Jellyfin/Kodi naming conventions plus the usual scene and
// fansub patterns:
//
//	Show (2020)/Season 01/Show - S01E02 - Title.mkv
//	Show/Show.S01E02.1080p.WEB-DL.x264-GRP.mkv
//	[Group] Show - 05 [1080p][ABCD1234].mkv
//	Show/Season 2/05 - Title.mkv
//	Movie (2010)/Movie (2010) - 1080p.mkv
//
// Go's regexp (RE2) has no lookahead or lookbehind. Where the patterns need
// a boundary, it is matched as a real character (or ^/$) outside a
// capturing "core" group, and the core's offsets are used as the match.
package naming

import (
	"regexp"
	"strconv"
	"strings"
)

var videoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".wmv": true,
	".ts": true, ".m2ts": true, ".mts": true, ".webm": true, ".flv": true, ".mpg": true,
	".mpeg": true, ".ogv": true, ".3gp": true, ".divx": true, ".vob": true,
}

// Directories that are never part of the library. NAS vendors litter shares
// with these (Synology @eaDir, QNAP .@__thumb, recycle bins, ...).
var ignoredDirs = map[string]bool{
	"@eadir": true, ".@__thumb": true, "#recycle": true, "#snapshot": true, "@recycle": true,
	".recycle": true, "$recycle.bin": true, "lost+found": true, ".streams": true,
	"extras": true, "extra": true, "featurettes": true, "behind the scenes": true,
	"deleted scenes": true, "interviews": true, "scenes": true, "shorts": true,
	"trailers": true, "trailer": true, "samples": true, "sample": true, "other": true,
	"bonus": true, "nc": true, "ncop": true, "nced": true, "menus": true, "scans": true,
	"cds": true, "sps": true,
}

// Jellyfin-style extras suffixes: "Movie-trailer.mkv", "Movie - sample.mkv"
var extraSuffix = regexp.MustCompile(`(?i)[ ._-](trailer|sample|featurette|behindthescenes|deleted|deletedscene|` +
	`interview|scene|short|extra|clip|other)\d*$`)
var sampleWord = regexp.MustCompile(`(?i)(^|[ ._\-\[(])sample([ ._\-\])]|$)`)

// Tokens that mark the start of release noise. Everything from the first one
// onward is dropped from a title.
const releaseTokens = `2160p|1080p|1080i|720p|576p|480p|4k|uhd|hdr10\+?|hdr|dv|dovi|` +
	`bluray|blu-ray|bdrip|brrip|bdremux|bd|remux|web-?dl|web-?rip|webrip|web|` +
	`hdtv|dvdrip|dvd|hdrip|amzn|nf|dsnp|hmax|atvp|cr|` +
	`x264|x265|h\.?264|h\.?265|hevc|avc|av1|xvid|divx|10bit|10-bit|8bit|hi10p?|` +
	`aac(?:2\.0|5\.1)?|ac3|eac3|e-ac-3|dts(?:-hd)?|truehd|atmos|flac|opus|ddp?5\.1|` +
	`dual[ .-]?audio|multi[ .-]?audio|multi|multisubs?|proper|repack|` +
	`uncensored|uncut|extended|internal|limited|complete|batch`

var (
	releaseRE    = regexp.MustCompile(`(?i)(?:^|[\s._\-\[(])(?:` + releaseTokens + `)(?:$|[\s._\-\])])`)
	yearParen    = regexp.MustCompile(`[(\[]((?:19|20)\d{2})[)\]]`)
	yearBare     = regexp.MustCompile(`(?:^|[\s.])((?:19|20)\d{2})`)
	brackets     = regexp.MustCompile(`\[[^\]]*\]|\{[^}]*\}`)
	emptyParens  = regexp.MustCompile(`\(\s*\)`)
	spaces       = regexp.MustCompile(`\s+`)
	trailingOpen = regexp.MustCompile(`[(\[]\s*$`)

	// Group 1 is the match proper; the characters around it stand in for
	// (?<![a-z0-9]) and (?![0-9]).
	sxxeyy = regexp.MustCompile(
		`(?i)(?:^|[^a-zA-Z0-9])(s(\d{1,2})[ ._-]{0,3}e(\d{1,4})(?:(?:[-_ ]?e|-)(\d{1,4}))?)(?:$|[^0-9])`)
	nxnn = regexp.MustCompile(
		`(?i)(?:^|[^a-zA-Z0-9])((\d{1,2})x(\d{2,3})(?:-(\d{2,3}))?)(?:$|[^0-9a-zA-Z])`)
	episodeWord = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z])((?:episode|ep\.?)[ ._-]?(\d{1,4}))(?:$|[^0-9])`)
	// A lone episode marker without a season: "Show.E01.1080p", "Show E12"
	eNumber = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])(e(\d{1,4})(?:v\d)?)(?:$|[^0-9a-zA-Z])`)
	// "Show - 05", "Show - 05v2", "Show - 05 - Title", "Show - 05 [1080p]"
	dashNumber = regexp.MustCompile(`^((.*?)\s+-\s+(\d{1,4})(?:v\d)?)(?:$|[\s\[(.\-])`)
	// Leading number inside a season/series folder: "05.mkv", "05 - Title.mkv"
	leadingNumber = regexp.MustCompile(`(?i)^((?:e|ep)?(\d{1,4})(?:v\d)?)(?:$|[\s._\-\[(])`)
	// Trailing number: "Show 05", "Show_05v2"
	trailingNumber = regexp.MustCompile(`^(.*?)[\s._](\d{1,3})(?:v\d)?$`)
	// Number followed by a title: "Naruto 001 - Enter Naruto"
	numberDash = regexp.MustCompile(`^(.*?\S)\s+(\d{1,4})(?:v\d)?\s+-\s+`)
	leadGroups = regexp.MustCompile(`^(?:\[[^\]]*\][\s_]*)+`)

	seasonDir    = regexp.MustCompile(`(?i)^(?:season|staffel|saison|temporada|series)[ ._-]*(\d{1,3})$|^s(\d{1,3})$`)
	specialsDir  = regexp.MustCompile(`(?i)^(?:specials?|season[ ._-]*0+|s0+|ova|ovas|oad)$`)
	seasonInName = regexp.MustCompile(
		`(?i)((?:^|[\s._\-])(?:s(\d{1,2})|season[ ._-]?(\d{1,2})))(?:$|[\s._\-\[(])`)
	genericStems = regexp.MustCompile(`(?i)^(movie|film|video|main|feature|title[ _]?t?\d*)$`)
	nonKey       = regexp.MustCompile(`[^0-9a-z\x{00C0}-\x{FFFF}]+`)
)

// Parsed is what a path says about the file it names.
type Parsed struct {
	Kind        string // "episode" | "movie"
	Title       string // movie title or episode title (may be "")
	Year        *int
	SeriesTitle string
	SeriesYear  *int
	SeriesDir   string // relative path of the series folder, "" for loose files
	Season      *int
	Episode     *int
	EpisodeEnd  *int
}

// SplitExt splits like Python's os.path.splitext: leading dots are not an extension.
func SplitExt(name string) (string, string) {
	sep := strings.LastIndex(name, "/")
	dot := strings.LastIndex(name, ".")
	if dot > sep {
		for i := sep + 1; i < dot; i++ {
			if name[i] != '.' {
				return name[:dot], name[dot:]
			}
		}
	}
	return name, ""
}

func IsVideo(name string) bool {
	_, ext := SplitExt(name)
	return videoExtensions[strings.ToLower(ext)]
}

func IsIgnoredDir(name string) bool {
	return strings.HasPrefix(name, ".") || ignoredDirs[strings.ToLower(name)]
}

// IsExtra reports trailers, samples and other bonus clips that should not be listed.
func IsExtra(filename string) bool {
	stem, _ := SplitExt(filename)
	return extraSuffix.MatchString(stem) || sampleWord.MatchString(stem)
}

func isYear(n int) bool { return n >= 1900 && n <= 2099 }

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func intp(n int) *int { return &n }

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func group(s string, m []int, i int) string {
	if m[2*i] < 0 {
		return ""
	}
	return s[m[2*i]:m[2*i+1]]
}

// CleanTitle strips release noise from a name. Returns (title, year).
//
// With allowEmpty, a name that is nothing but release noise becomes "".
func CleanTitle(raw string, allowEmpty bool) (string, *int) {
	s := raw
	if !strings.Contains(s, " ") && strings.Contains(s, ".") {
		s = strings.ReplaceAll(s, ".", " ")
	}
	s = strings.ReplaceAll(s, "_", " ")
	s = brackets.ReplaceAllString(s, " ")

	var year *int
	if m := yearParen.FindStringSubmatchIndex(s); m != nil {
		year = intp(atoi(s[m[2]:m[3]]))
		s = s[:m[0]] + " " + s[m[1]:]
	}

	if m := releaseRE.FindStringIndex(s); m != nil && (m[0] > 0 || allowEmpty) {
		s = s[:m[0]]
	}

	if year == nil {
		// A bare year like "Blade Runner 2049 2017" -> take the last year,
		// but never consume the whole title ("1917", "2012").
		var last []int
		for _, m := range yearBare.FindAllStringSubmatchIndex(s, -1) {
			if end := m[1]; end == len(s) || s[end] == ' ' || s[end] == '.' || isSpace(s[end]) {
				last = m
			}
		}
		if last != nil {
			before := strings.Trim(s[:last[0]], " -.")
			if before != "" {
				year = intp(atoi(s[last[2]:last[3]]))
				s = before + s[last[1]:]
			}
		}
	}

	s = emptyParens.ReplaceAllString(s, " ")
	s = strings.Trim(spaces.ReplaceAllString(s, " "), " -._,")
	// Unbalanced trailing "(" left over from a cut.
	s = strings.Trim(trailingOpen.ReplaceAllString(s, ""), " -._,")
	return s, year
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// SeasonFromDir is the season number if name is a pure season folder
// ("Season 01", "S2", "Specials").
func SeasonFromDir(name string) *int {
	n := strings.TrimSpace(name)
	if specialsDir.MatchString(n) {
		return intp(0)
	}
	if m := seasonDir.FindStringSubmatch(n); m != nil {
		if m[1] != "" {
			return intp(atoi(m[1]))
		}
		return intp(atoi(m[2]))
	}
	return nil
}

// SplitSeasonInName turns "Show.S02.1080p.BluRay-GRP" into ("Show", 2).
// Leaves names without a season alone.
func SplitSeasonInName(name string) (string, *int) {
	m := seasonInName.FindStringSubmatchIndex(name)
	if m == nil {
		return name, nil
	}
	start := m[2]
	// Must not be an SxxEyy episode marker.
	if e := sxxeyy.FindStringSubmatchIndex(name[start:]); e != nil && e[2] <= 1 {
		return name, nil
	}
	n := group(name, m, 2)
	if n == "" {
		n = group(name, m, 3)
	}
	return name[:start], intp(atoi(n))
}

// SeasonInDir is the season named by a folder, pure ("Season 2") or not
// ("Show.S02.1080p", "Season 1 (2019)").
func SeasonInDir(name string) *int {
	if s := SeasonFromDir(name); s != nil {
		return s
	}
	_, s := SplitSeasonInName(name)
	return s
}

func episodeTitle(rest string) string {
	rest = strings.TrimSpace(rest)
	if !strings.Contains(rest, " ") && strings.Contains(rest, ".") {
		rest = strings.ReplaceAll(rest, ".", " ")
	}
	rest = strings.ReplaceAll(rest, "_", " ")
	rest = strings.TrimLeft(rest, " -.")
	title, _ := CleanTitle(rest, true)
	return title
}

type found struct {
	prefix                 string
	season, episode, epEnd *int
	rest                   string
}

func optInt(s string) *int {
	if s == "" {
		return nil
	}
	return intp(atoi(s))
}

// matchEpisode finds an episode marker.
func matchEpisode(stem string) *found {
	if m := sxxeyy.FindStringSubmatchIndex(stem); m != nil {
		if m[8] >= 0 && m[9] < len(stem) && isASCIILetter(stem[m[9]]) {
			// "S01E01-1080p", "S01E05-720p.WEB": a resolution, not a range end.
			return &found{stem[:m[2]], intp(atoi(group(stem, m, 2))), intp(atoi(group(stem, m, 3))),
				nil, stem[m[7]:]}
		}
		return &found{stem[:m[2]], intp(atoi(group(stem, m, 2))), intp(atoi(group(stem, m, 3))),
			optInt(group(stem, m, 4)), stem[m[3]:]}
	}
	if m := nxnn.FindStringSubmatchIndex(stem); m != nil && !isYear(atoi(group(stem, m, 2)+group(stem, m, 3))) {
		return &found{stem[:m[2]], intp(atoi(group(stem, m, 2))), intp(atoi(group(stem, m, 3))),
			optInt(group(stem, m, 4)), stem[m[3]:]}
	}
	if m := episodeWord.FindStringSubmatchIndex(stem); m != nil {
		return &found{stem[:m[2]], nil, intp(atoi(group(stem, m, 2))), nil, stem[m[3]:]}
	}
	if m := eNumber.FindStringSubmatchIndex(stem); m != nil && m[2] > 0 {
		return &found{stem[:m[2]], nil, intp(atoi(group(stem, m, 2))), nil, stem[m[3]:]}
	}
	// Fansub style. Strip leading [Group] first so it does not end up in the title.
	// Underscores stand in for spaces: "[Grp]_Show_-_01_[720p]".
	body := stem
	if lead := leadGroups.FindStringIndex(stem); lead != nil {
		body = stem[lead[1]:]
	}
	if !strings.Contains(body, " ") {
		body = strings.ReplaceAll(body, "_", " ")
	}
	if m := dashNumber.FindStringSubmatchIndex(body); m != nil && !isYear(atoi(group(body, m, 3))) {
		return &found{group(body, m, 2), nil, intp(atoi(group(body, m, 3))), nil, body[m[3]:]}
	}
	return nil
}

// ParsePath parses a path relative to the library root.
//
// libraryType is "auto", "shows" or "movies". siblings is the number of
// video files in the same directory; it gates the weakest heuristics (bare
// numbers) so a lone "Movie 2.mkv" is not turned into an episode.
func ParsePath(relPath, libraryType string, siblings int) Parsed {
	relPath = strings.Trim(strings.ReplaceAll(relPath, `\`, "/"), "/")
	parts := strings.Split(relPath, "/")
	filename := parts[len(parts)-1]
	dirs := parts[:len(parts)-1]
	stem, _ := SplitExt(filename)

	if libraryType != "movies" {
		if ep := parseEpisode(stem, dirs, libraryType, siblings); ep != nil {
			return *ep
		}
	}
	return parseMovie(stem, dirs)
}

// seriesFromDirs works out (title, year, dir, season) from the folder chain.
//
// The series is the top-level folder inside the library, however deep the
// file sits below it ("Show/Show S02/Disc 1/x.mkv" belongs to "Show"), so
// every episode under one show folder lands in one series. The season comes
// from the deepest folder that names one ("Season 2", "Show.S02.1080p").
// ok is false when there is no series folder.
func seriesFromDirs(dirs []string) (title string, year *int, dir string, season *int, ok bool) {
	if len(dirs) == 0 {
		return "", nil, "", nil, false
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if season = SeasonInDir(dirs[i]); season != nil {
			break
		}
	}
	if SeasonFromDir(dirs[0]) != nil {
		// The library itself is one show ("Season 1/x.mkv"): no series folder.
		return "", nil, "", season, false
	}
	name := dirs[0]
	stripped, _ := SplitSeasonInName(name)
	title, year = CleanTitle(stripped, false)
	if title == "" {
		title, year = CleanTitle(name, false)
	}
	return title, year, name, season, true
}

// SeriesFromDir is the (title, year) of the series a top-level library
// folder would hold; ok is false when the folder is a season folder, not a show.
func SeriesFromDir(name string) (string, *int, bool) {
	title, year, _, _, ok := seriesFromDirs([]string{name})
	return title, year, ok
}

func parseEpisode(stem string, dirs []string, libraryType string, siblings int) *Parsed {
	f := matchEpisode(stem)
	if f == nil && len(dirs) >= 2 {
		// Per-episode folder with an obfuscated file inside:
		// "Show/Show.S01E02.1080p-GRP/abc123.mkv", "Show/Episode 01/video.mkv".
		if d := matchEpisode(dirs[len(dirs)-1]); d != nil {
			f = &found{"", d.season, d.episode, d.epEnd, ""}
		}
	}
	// Any folder naming a season ("Season 1 (2019)", "Show.S01.1080p-GRP")
	// means everything below it is an episode, numbered or not.
	underSeason := false
	for _, d := range dirs {
		underSeason = underSeason || SeasonInDir(d) != nil
	}

	if f == nil {
		// Bare numbers only when context says this is a series.
		if len(dirs) > 0 && (underSeason || siblings >= 2 || libraryType == "shows") {
			if m := leadingNumber.FindStringSubmatchIndex(stem); m != nil && !isYear(atoi(group(stem, m, 2))) {
				f = &found{"", nil, intp(atoi(group(stem, m, 2))), nil, stem[m[3]:]}
			} else if m := numberDash.FindStringSubmatchIndex(stem); m != nil && !isYear(atoi(group(stem, m, 2))) {
				f = &found{group(stem, m, 1), nil, intp(atoi(group(stem, m, 2))), nil, stem[m[1]:]}
			}
			if f == nil {
				cleaned := strings.TrimSpace(brackets.ReplaceAllString(stem, " "))
				if cut := releaseRE.FindStringIndex(cleaned); cut != nil && cut[0] > 0 {
					cleaned = cleaned[:cut[0]]
				}
				t := strings.Trim(cleaned, " ._-")
				if m := trailingNumber.FindStringSubmatch(t); m != nil && !isYear(atoi(m[2])) {
					f = &found{m[1], nil, intp(atoi(m[2])), nil, ""}
				}
			}
		}
		if f == nil && libraryType != "shows" && !underSeason {
			return nil
		}
		if f == nil {
			f = &found{stem, nil, nil, nil, ""}
		}
	}

	prefix, season := f.prefix, f.season
	var nameSeason *int
	if season == nil {
		// "[Grp] Show S2 - 05": the season sits in front of the bare number.
		stripped, ns := SplitSeasonInName(prefix)
		if t, _ := CleanTitle(stripped, false); ns != nil && t != "" {
			prefix, nameSeason = stripped, ns
		}
	}
	sTitle, sYear, sDir, dirSeason, ok := seriesFromDirs(dirs)
	if !ok {
		sTitle, sYear = CleanTitle(prefix, false)
		sDir = ""
		if sTitle == "" {
			sTitle = "Unknown"
		}
	}
	if season == nil {
		switch {
		case dirSeason != nil:
			season = dirSeason
		case nameSeason != nil:
			season = nameSeason
		default:
			season = intp(1)
		}
	}
	title := ""
	if f.rest != "" {
		title = episodeTitle(f.rest)
	}
	if f.episode == nil {
		title, _ = CleanTitle(stem, false)
	}
	return &Parsed{
		Kind: "episode", Title: title, SeriesTitle: sTitle, SeriesYear: sYear, SeriesDir: sDir,
		Season: season, Episode: f.episode, EpisodeEnd: f.epEnd,
	}
}

func parseMovie(stem string, dirs []string) Parsed {
	title, year := CleanTitle(stem, false)
	if len(dirs) > 0 {
		dTitle, dYear := CleanTitle(dirs[len(dirs)-1], false)
		if title == "" || genericStems.MatchString(title) {
			if dTitle != "" {
				title = dTitle
			}
		}
		if year == nil {
			year = dYear
		}
	}
	if title == "" {
		title = stem
	}
	return Parsed{Kind: "movie", Title: title, Year: year}
}

// SortKey drops a leading article: "The Matrix" sorts under M.
func SortKey(title string) string {
	t := strings.ToLower(title)
	for _, article := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(t, article) {
			return t[len(article):]
		}
	}
	return t
}

// TitleKey is the normalised key used to merge files belonging to the same series.
func TitleKey(title string) string {
	return nonKey.ReplaceAllString(strings.ToLower(title), "")
}
