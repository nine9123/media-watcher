// Package playback decides which audio track to play, and direct play vs
// remux vs transcode.
//
// The order of preference is chosen for a small NAS CPU:
//
//	direct     the file is already browser-playable (mp4/webm, playable codecs,
//	           wanted audio track is the first one). Served with byte ranges,
//	           zero CPU.
//	remux      video is playable but the container is not (mkv, avi, ts...) or
//	           the wanted audio track is not the first one. Video is copied,
//	           audio copied or converted to AAC. Costs almost nothing.
//	transcode  video codec is not playable in this browser (HEVC on most
//	           desktop browsers, 10-bit H.264, MPEG-2, VC-1, ...). Only then
//	           do we encode video.
//
// Subtitles are never mapped, in any mode.
package playback

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/naming"
	"github.com/nine9123/media-watcher/internal/probe"
)

// Codecs every target browser plays in MP4. Anything else must be advertised
// by the client in its capability list.
var (
	baseVideo    = []string{"h264"}
	baseAudio    = []string{"aac", "mp3"}
	h264OKPix    = map[string]bool{"yuv420p": true, "yuvj420p": true}
	hdrTransfers = map[string]bool{"smpte2084": true, "arib-std-b67": true}
	mp4Exts      = map[string]bool{".mp4": true, ".m4v": true}
)

// Tonemap takes HDR (PQ/HLG) to SDR BT.709, so tone-mapped H.264 does not look washed out.
const Tonemap = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709," +
	"tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv"

// Media is what playback needs to know about a file.
type Media struct {
	Path      string
	Container string
	Duration  *float64
	Streams   []probe.Stream
}

func (m *Media) byIndex(i *int) *probe.Stream {
	if i == nil {
		return nil
	}
	for k := range m.Streams {
		if m.Streams[k].Index == *i {
			return &m.Streams[k]
		}
	}
	return nil
}

// Caps are the codecs the browser says it decodes.
type Caps struct {
	Video map[string]bool
	Audio map[string]bool
}

func set(base []string, csv string) map[string]bool {
	out := map[string]bool{}
	for _, x := range base {
		out[x] = true
	}
	for _, x := range strings.Split(csv, ",") {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
			out[x] = true
		}
	}
	return out
}

// ParseCaps reads the vcaps/acaps query parameters.
func ParseCaps(video, audio string) Caps {
	return Caps{Video: set(baseVideo, video), Audio: set(baseAudio, audio)}
}

// DefaultCaps is what every browser plays.
func DefaultCaps() Caps { return ParseCaps("", "") }

type Decision struct {
	Mode       string // direct | remux | transcode
	VideoIndex *int
	AudioIndex *int
	Video      string // copy | transcode | none
	Audio      string // copy | aac | none
	Reason     string
}

func AudioStreams(streams []probe.Stream) []*probe.Stream {
	var out []*probe.Stream
	for i := range streams {
		if streams[i].Type == "audio" {
			out = append(out, &streams[i])
		}
	}
	return out
}

func langMatch(s *probe.Stream, langs []string) bool {
	lang := strings.ToLower(s.Language)
	title := strings.ToLower(s.Title)
	for _, l := range langs {
		if lang != "" && lang == l {
			return true
		}
	}
	for _, l := range langs {
		if len([]rune(l)) > 3 && strings.Contains(title, l) {
			return true
		}
	}
	return false
}

func pickFrom(audio []*probe.Stream, langs []string) *probe.Stream {
	if len(audio) == 0 {
		return nil
	}
	ranked := append([]*probe.Stream(nil), audio...)
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	key := func(s *probe.Stream) [3]int { return [3]int{b(!langMatch(s, langs)), b(s.Comment), b(!s.Default)} }
	sort.SliceStable(ranked, func(i, j int) bool {
		a, c := key(ranked[i]), key(ranked[j])
		for k := 0; k < 3; k++ {
			if a[k] != c[k] {
				return a[k] < c[k]
			}
		}
		return false
	})
	return ranked[0]
}

// PickAudio: preferred language first, then non-commentary, then default flag, then file order.
func PickAudio(streams []probe.Stream, langs []string) *probe.Stream {
	return pickFrom(AudioStreams(streams), langs)
}

func VideoPlayable(v *probe.Stream, caps Caps) bool {
	if v == nil {
		return true
	}
	pix := v.PixFmtString()
	tenBit := strings.Contains(pix, "10") || strings.Contains(pix, "12")
	switch v.Codec {
	case "h264":
		profile := strings.ToLower(v.ProfileString())
		return h264OKPix[pix] && !strings.Contains(profile, "10") && !strings.Contains(profile, "4:")
	case "hevc":
		if !caps.Video["hevc"] {
			return false
		}
		return !tenBit || caps.Video["hevc10"]
	case "av1":
		return caps.Video["av1"]
	case "vp9":
		return caps.Video["vp9"]
	}
	return false
}

func audioCopyable(a *probe.Stream, caps Caps) bool { return a == nil || caps.Audio[a.Codec] }

func ext(path string) string {
	_, e := naming.SplitExt(filepath.Base(path))
	return strings.ToLower(e)
}

// Decide picks the mode. force is "remux" (never direct) or "transcode"
// (encode everything); the player uses it to step down when the browser
// fails on a file it claimed to support.
func Decide(m *Media, caps Caps, langs []string, audioIndex *int, force string) Decision {
	d := decide(m, caps, langs, audioIndex)
	if force == "transcode" && d.VideoIndex != nil {
		a := "aac"
		if d.AudioIndex == nil {
			a = "none"
		}
		return Decision{"transcode", d.VideoIndex, d.AudioIndex, "transcode", a, "forced transcode"}
	}
	if (force == "remux" || force == "transcode") && d.Mode == "direct" {
		return Decision{"remux", d.VideoIndex, d.AudioIndex, d.Video, d.Audio, "forced remux"}
	}
	return d
}

func decide(m *Media, caps Caps, langs []string, audioIndex *int) Decision {
	var video *probe.Stream
	for i := range m.Streams {
		if m.Streams[i].Type == "video" {
			video = &m.Streams[i]
			break
		}
	}
	audios := AudioStreams(m.Streams)
	var chosen *probe.Stream
	if audioIndex != nil {
		for _, s := range audios {
			if s.Index == *audioIndex {
				chosen = s
				break
			}
		}
	}
	if chosen == nil {
		chosen = pickFrom(audios, langs)
	}

	var vIdx, aIdx *int
	if video != nil {
		vIdx = &video.Index
	}
	if chosen != nil {
		aIdx = &chosen.Index
	}
	vOK := VideoPlayable(video, caps)
	aOK := audioCopyable(chosen, caps)
	aMode := "none"
	if chosen != nil {
		aMode = map[bool]string{true: "copy", false: "aac"}[aOK]
	}
	vMode := "none"
	if video != nil {
		vMode = map[bool]string{true: "copy", false: "transcode"}[vOK]
	}

	e := ext(m.Path)
	isMP4 := mp4Exts[e] && strings.Contains(m.Container, "mp4")
	firstAudio := chosen == nil || chosen == audios[0]

	if vMode == "transcode" {
		return Decision{"transcode", vIdx, aIdx, vMode, aMode,
			fmt.Sprintf("video %s %s not playable", video.Codec, pyStr(video.PixFmt))}
	}
	if isMP4 && vOK && aOK && firstAudio {
		return Decision{"direct", vIdx, aIdx, vMode, aMode, "file is browser-playable"}
	}
	if e == ".webm" && firstAudio && video != nil &&
		(video.Codec == "vp8" || video.Codec == "vp9" || video.Codec == "av1") &&
		(chosen == nil || chosen.Codec == "opus" || chosen.Codec == "vorbis") {
		return Decision{"direct", vIdx, aIdx, vMode, aMode, "webm is browser-playable"}
	}
	var reasons []string
	if !isMP4 {
		c := e
		if c == "" {
			c = m.Container
		}
		reasons = append(reasons, "container "+c)
	}
	if !firstAudio {
		reasons = append(reasons, "selected audio is not the first track")
	}
	if !aOK {
		reasons = append(reasons, "audio "+chosen.Codec)
	}
	reason := strings.Join(reasons, ", ")
	if reason == "" {
		reason = "remux"
	}
	return Decision{"remux", vIdx, aIdx, vMode, aMode, reason}
}

// pyStr prints like Python's str(): None for a missing value.
func pyStr(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}

func audioArgs(cfg *config.Config, a *probe.Stream, mode string) []string {
	switch mode {
	case "none":
		return []string{"-an"}
	case "copy":
		return []string{"-c:a", "copy"}
	}
	src := 2
	if a != nil && a.ChannelsInt() > 0 {
		src = a.ChannelsInt()
	}
	ch := max(1, min(src, cfg.AudioChannels))
	bitrate := "192k"
	if ch > 2 {
		bitrate = "448k"
	}
	return []string{"-c:a", "aac", "-b:a", bitrate, "-ac", strconv.Itoa(ch)}
}

func targetHeight(cfg *config.Config, height int) int {
	if height > 0 && cfg.MaxHeight > 0 && height > cfg.MaxHeight {
		return cfg.MaxHeight
	}
	return 0
}

func transferHDR(s *probe.Stream) bool {
	return s != nil && s.ColorTransfer != nil && hdrTransfers[*s.ColorTransfer]
}

// FFmpegArgs is the command line producing a fragmented MP4 on stdout.
func FFmpegArgs(cfg *config.Config, m *Media, d Decision, start float64) []string {
	video := m.byIndex(d.VideoIndex)
	audio := m.byIndex(d.AudioIndex)
	vaapi := d.Video == "transcode" && cfg.HWAccel == "vaapi"

	args := []string{cfg.FFmpeg, "-hide_banner", "-loglevel", "error", "-nostdin"}
	if vaapi {
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_device", cfg.VAAPIDevice,
			"-hwaccel_output_format", "vaapi")
	}
	if start > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", start))
	}
	args = append(args, "-i", m.Path)
	if d.VideoIndex != nil {
		args = append(args, "-map", fmt.Sprintf("0:%d", *d.VideoIndex))
	}
	if d.AudioIndex != nil {
		args = append(args, "-map", fmt.Sprintf("0:%d", *d.AudioIndex))
	}
	args = append(args, "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1")

	switch d.Video {
	case "copy":
		args = append(args, "-c:v", "copy")
		if video != nil && video.Codec == "hevc" {
			args = append(args, "-tag:v", "hvc1") // Safari/Chrome want hvc1, not hev1
		}
	case "transcode":
		h := 0
		if video != nil {
			h = targetHeight(cfg, video.HeightInt())
		}
		if vaapi {
			scale := "scale_vaapi=format=nv12"
			if h > 0 {
				scale = fmt.Sprintf("scale_vaapi=w=-2:h=%d:format=nv12", h)
			}
			args = append(args, "-vf", scale, "-c:v", "h264_vaapi", "-qp", strconv.Itoa(cfg.VideoCRF+1))
		} else {
			var filters []string
			if h > 0 {
				filters = append(filters, fmt.Sprintf("scale=-2:%d", h))
			}
			if transferHDR(video) {
				filters = append(filters, Tonemap)
			}
			filters = append(filters, "format=yuv420p")
			args = append(args,
				"-vf", strings.Join(filters, ","),
				"-c:v", "libx264", "-preset", cfg.X264Preset, "-crf", strconv.Itoa(cfg.VideoCRF),
				"-maxrate", "20M", "-bufsize", "40M", "-profile:v", "high",
				// Every x264 frame thread and lookahead frame holds its own
				// buffers; left alone they are the largest memory user here.
				"-rc-lookahead", "10",
			)
			if cfg.TranscodeThreads > 0 {
				args = append(args, "-threads", strconv.Itoa(cfg.TranscodeThreads))
			}
		}
		// Short GOP so the stream starts quickly and fragments stay small.
		args = append(args, "-g", "48", "-keyint_min", "48", "-sc_threshold", "0")
	}

	args = append(args, audioArgs(cfg, audio, d.Audio)...)
	return append(args,
		"-avoid_negative_ts", "make_zero",
		"-max_muxing_queue_size", "4096",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-frag_duration", "2000000",
		"-f", "mp4", "pipe:1",
	)
}

// RFC 6381 codec ids for the MIME type a Media Source Extensions player must
// declare. Decoders configure themselves from the init segment, so the level
// only has to be one the browser accepts; it is picked from the picture size.
var (
	audioCodecIDs = map[string]string{"aac": "mp4a.40.2", "mp3": "mp4a.6B", "ac3": "ac-3",
		"eac3": "ec-3", "opus": "opus", "flac": "flac"}
	h264ProfileIDs = map[string]string{"baseline": "4200", "constrained baseline": "42E0",
		"main": "4D40", "high": "6400"}
)

// StreamMIME is the MIME type with codecs of the fragmented MP4 that FFmpegArgs produces.
func StreamMIME(cfg *config.Config, m *Media, d Decision) string {
	video := m.byIndex(d.VideoIndex)
	if video == nil {
		video = &probe.Stream{}
	}
	audio := m.byIndex(d.AudioIndex)
	if audio == nil {
		audio = &probe.Stream{}
	}
	var codecs []string
	level := func(h int) string {
		if h > 1080 {
			return "33"
		}
		return "28"
	}
	switch d.Video {
	case "transcode":
		h := targetHeight(cfg, video.HeightInt())
		if h == 0 {
			h = video.HeightInt()
		}
		codecs = append(codecs, "avc1.6400"+level(h))
	case "copy":
		h := video.HeightInt()
		pix := video.PixFmtString()
		tenBit := strings.Contains(pix, "10") || strings.Contains(pix, "12")
		switch video.Codec {
		case "h264":
			profile, ok := h264ProfileIDs[strings.ToLower(video.ProfileString())]
			if !ok {
				profile = "6400"
			}
			codecs = append(codecs, "avc1."+profile+level(h))
		case "hevc":
			lv := "L120"
			if h > 1080 {
				lv = "L153"
			}
			if tenBit {
				codecs = append(codecs, "hvc1.2.4."+lv+".90")
			} else {
				codecs = append(codecs, "hvc1.1.6."+lv+".90")
			}
		case "av1":
			codecs = append(codecs, map[bool]string{true: "av01.0.08M.10", false: "av01.0.08M.08"}[tenBit])
		case "vp9":
			codecs = append(codecs, map[bool]string{true: "vp09.02.40.10", false: "vp09.00.40.08"}[tenBit])
		}
	}
	switch d.Audio {
	case "aac":
		codecs = append(codecs, "mp4a.40.2")
	case "copy":
		id, ok := audioCodecIDs[audio.Codec]
		if !ok {
			id = "mp4a.40.2"
		}
		codecs = append(codecs, id)
	}
	if len(codecs) == 0 {
		return "video/mp4"
	}
	return `video/mp4; codecs="` + strings.Join(codecs, ",") + `"`
}

// shellQuote quotes like Python's shlex.quote.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// ConvertCommand is a one-off shell command that re-encodes the video to 8-bit H.264.
//
// Every audio track (and, from MKV, every subtitle and font) is copied, so
// afterwards the file only ever needs a remux instead of a CPU transcode.
// The result is written to a temporary name first; an MKV is then replaced
// in place, which keeps its item id and watch progress. Other containers get
// a new .mkv next to the original.
func ConvertCommand(m *Media, d Decision) string {
	video := m.byIndex(d.VideoIndex)
	src := filepath.Base(m.Path)
	stem, e := naming.SplitExt(src)
	dst := stem + ".mkv"
	vi := "None"
	if d.VideoIndex != nil {
		vi = strconv.Itoa(*d.VideoIndex)
	}
	args := []string{"ffmpeg", "-hide_banner", "-i", src, "-map", "0:" + vi, "-map", "0:a?"}
	if strings.ToLower(e) == ".mkv" {
		args = append(args, "-map", "0:s?", "-map", "0:t?")
	}
	args = append(args, "-c", "copy", "-c:v", "libx264", "-preset", "slow", "-crf", "18",
		"-profile:v", "high")
	var filters []string
	if transferHDR(video) {
		filters = append(filters, Tonemap)
	}
	filters = append(filters, "format=yuv420p")
	args = append(args, "-vf", strings.Join(filters, ","), "-f", "matroska", dst+".part")
	return shellJoin(args) + " && mv -- " + shellJoin([]string{dst + ".part", dst})
}

var (
	copyAudio = map[string]bool{"aac": true, "mp3": true} // what browsers play from the converted copy as is
	english   = []string{"eng", "en", "english"}
)

// ConvertCRF is visually transparent; live transcodes use VIDEO_CRF.
const ConvertCRF = 18

// ConvertedAudio is the one audio track of a converted copy: the preferred
// language (AUDIO_LANGUAGES), else English, else whatever PickAudio would play.
func ConvertedAudio(streams []probe.Stream, langs []string) *probe.Stream {
	audios := AudioStreams(streams)
	for _, wanted := range [][]string{langs, english} {
		var match []*probe.Stream
		for _, s := range audios {
			if langMatch(s, wanted) {
				match = append(match, s)
			}
		}
		if len(match) > 0 {
			return pickFrom(match, wanted)
		}
	}
	return pickFrom(audios, langs)
}

// ConvertedArgs is the ffmpeg command writing a browser-playable MP4 copy of m to out.
//
// 8-bit H.264 at the source resolution (HDR tone-mapped, ConvertCRF, not
// the live MAX_HEIGHT/VIDEO_CRF) with a single audio track (see
// ConvertedAudio), and faststart so the copy is served with byte ranges
// like any direct-play file. Chapters (skip intro) are kept; subtitles are
// not, MP4 cannot carry the usual ASS/PGS ones.
// Progress is written to stdout as key=value lines.
func ConvertedArgs(cfg *config.Config, m *Media, langs []string, out string) ([]string, error) {
	var video *probe.Stream
	for i := range m.Streams {
		if m.Streams[i].Type == "video" {
			video = &m.Streams[i]
			break
		}
	}
	if video == nil {
		return nil, errors.New("no video stream")
	}
	audio := ConvertedAudio(m.Streams, langs)

	args := []string{cfg.FFmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-nostats",
		"-progress", "pipe:1", "-y", "-i", m.Path, "-map", fmt.Sprintf("0:%d", video.Index)}
	if audio != nil {
		args = append(args, "-map", fmt.Sprintf("0:%d", audio.Index))
	}
	args = append(args, "-sn", "-dn")

	var filters []string
	if transferHDR(video) {
		filters = append(filters, Tonemap)
	}
	filters = append(filters, "format=yuv420p")
	args = append(args, "-vf", strings.Join(filters, ","), "-c:v", "libx264", "-preset", cfg.X264Preset,
		"-crf", strconv.Itoa(ConvertCRF), "-profile:v", "high", "-rc-lookahead", "10")
	if cfg.TranscodeThreads > 0 {
		args = append(args, "-threads", strconv.Itoa(cfg.TranscodeThreads))
	}

	switch {
	case audio == nil:
		args = append(args, "-an")
	case copyAudio[audio.Codec]:
		args = append(args, "-c:a", "copy")
	default:
		src := audio.ChannelsInt()
		if src == 0 {
			src = 2
		}
		ch := max(1, min(src, cfg.AudioChannels))
		bitrate := "192k"
		if ch > 2 {
			bitrate = "448k"
		}
		args = append(args, "-c:a", "aac", "-b:a", bitrate, "-ac", strconv.Itoa(ch))
	}
	if audio != nil {
		args = append(args, "-disposition:a:0", "default")
	}
	return append(args, "-max_muxing_queue_size", "4096", "-movflags", "+faststart", "-f", "mp4", out), nil
}

// AudioTrack is one entry of the player's audio menu.
type AudioTrack struct {
	Index    int    `json:"index"`
	Label    string `json:"label"`
	Language string `json:"language"`
}

func DescribeAudio(streams []probe.Stream) []AudioTrack {
	out := []AudioTrack{}
	for _, s := range AudioStreams(streams) {
		var parts []string
		if s.Language != "" {
			parts = append(parts, strings.ToUpper(s.Language))
		}
		if s.Title != "" {
			parts = append(parts, s.Title)
		}
		ch := "?"
		if s.ChannelsInt() > 0 {
			ch = strconv.Itoa(s.ChannelsInt())
		}
		parts = append(parts, fmt.Sprintf("%s %sch", s.Codec, ch))
		out = append(out, AudioTrack{s.Index, strings.Join(parts, " · "), s.Language})
	}
	return out
}
