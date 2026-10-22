package playback

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/probe"
)

var JA = []string{"jpn", "ja", "japanese"}

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

type vopt func(*probe.Stream)

func v(codec, pix, profile string, index, height int, opts ...vopt) probe.Stream {
	w := height * 16 / 9
	s := probe.Stream{Index: index, Type: "video", Codec: codec, PixFmt: sp(pix), Profile: sp(profile),
		Height: ip(height), Width: &w}
	for _, o := range opts {
		o(&s)
	}
	return s
}

func vd() probe.Stream { return v("h264", "yuv420p", "High", 0, 1080) }

func hdr(s *probe.Stream) { s.ColorTransfer = sp("smpte2084") }

type aopt struct {
	codec, lang, title string
	def, comment       bool
	channels           int
}

func a(index int, o aopt) probe.Stream {
	if o.codec == "" {
		o.codec = "aac"
	}
	if o.channels == 0 {
		o.channels = 2
	}
	return probe.Stream{Index: index, Type: "audio", Codec: o.codec, Language: o.lang, Title: o.title,
		Default: o.def, Comment: o.comment, Channels: ip(o.channels)}
}

func item(path, container string, streams ...probe.Stream) *Media {
	if path == "" {
		path = "/m/x.mkv"
	}
	if container == "" {
		container = "matroska,webm"
	}
	return &Media{Path: path, Container: container, Streams: streams}
}

func cfg(mod ...func(*config.Config)) *config.Config {
	c := config.Defaults()
	c.APIKey = "xxxxxxxx"
	for _, m := range mod {
		m(&c)
	}
	return &c
}

func argAfter(args []string, flag string) string {
	for i, x := range args {
		if x == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func index(args []string, flag string) int {
	for i, x := range args {
		if x == flag {
			return i
		}
	}
	return -1
}

func maps(args []string) []string {
	var out []string
	for i, x := range args {
		if x == "-map" {
			out = append(out, args[i+1])
		}
	}
	return out
}

func has(args []string, x string) bool { return index(args, x) >= 0 }

func TestPickAudio(t *testing.T) {
	if p := PickAudio([]probe.Stream{vd(), a(1, aopt{lang: "eng", def: true}), a(2, aopt{lang: "jpn"})}, JA); p.Index != 2 {
		t.Error("japanese should win over default english")
	}
	if p := PickAudio([]probe.Stream{vd(), a(1, aopt{title: "English 5.1", def: true}), a(2, aopt{title: "Japanese 2.0"})}, JA); p.Index != 2 {
		t.Error("japanese by title")
	}
	if p := PickAudio([]probe.Stream{vd(), a(1, aopt{lang: "jpn", title: "Commentary", comment: true}), a(2, aopt{lang: "jpn"})}, JA); p.Index != 2 {
		t.Error("commentary is last resort")
	}
	if p := PickAudio([]probe.Stream{vd(), a(1, aopt{lang: "eng"}), a(2, aopt{lang: "ger", def: true})}, JA); p.Index != 2 {
		t.Error("default flag")
	}
	if p := PickAudio([]probe.Stream{vd(), a(1, aopt{lang: "eng"}), a(2, aopt{lang: "ger"})}, JA); p.Index != 1 {
		t.Error("first")
	}
	if PickAudio([]probe.Stream{vd()}, JA) != nil {
		t.Error("no audio")
	}
}

func TestDecide(t *testing.T) {
	mp4 := item("/m/x.mp4", "mov,mp4,m4a,3gp,3g2,mj2", vd(), a(1, aopt{lang: "jpn"}))
	if d := Decide(mp4, DefaultCaps(), JA, nil, ""); d.Mode != "direct" {
		t.Error(d)
	}
	mp4b := item("/m/x.mp4", "mov,mp4,m4a,3gp,3g2,mj2", vd(), a(1, aopt{lang: "eng"}), a(2, aopt{lang: "jpn"}))
	d := Decide(mp4b, DefaultCaps(), JA, nil, "")
	if d.Mode != "remux" || *d.AudioIndex != 2 || d.Video != "copy" || d.Audio != "copy" {
		t.Error(d)
	}
	ac3 := item("", "", vd(), a(1, aopt{codec: "ac3", channels: 6}))
	if d := Decide(ac3, DefaultCaps(), JA, nil, ""); d.Mode != "remux" || d.Video != "copy" || d.Audio != "aac" {
		t.Error(d)
	}
	if d := Decide(item("", "", vd(), a(1, aopt{codec: "ac3"})), ParseCaps("", "ac3"), JA, nil, ""); d.Audio != "copy" {
		t.Error(d)
	}
}

func TestHEVCDependsOnBrowser(t *testing.T) {
	it := item("", "", v("hevc", "yuv420p10le", "Main 10", 0, 1080), a(1, aopt{codec: "flac"}))
	if Decide(it, DefaultCaps(), JA, nil, "").Mode != "transcode" || Decide(it, ParseCaps("hevc", ""), JA, nil, "").Mode != "transcode" {
		t.Error("hevc10 without caps")
	}
	d := Decide(it, ParseCaps("hevc,hevc10", "flac"), JA, nil, "")
	if d.Mode != "remux" || d.Video != "copy" || d.Audio != "copy" {
		t.Error(d)
	}
	if d := Decide(item("", "", v("h264", "yuv420p10le", "High 10", 0, 1080), a(1, aopt{})), DefaultCaps(), JA, nil, ""); d.Mode != "transcode" {
		t.Error("hi10p")
	}
	for _, c := range []string{"mpeg2video", "vc1", "mpeg4", "msmpeg4v3"} {
		if d := Decide(item("", "", v(c, "yuv420p", "High", 0, 1080), a(1, aopt{})), DefaultCaps(), JA, nil, ""); d.Mode != "transcode" {
			t.Error(c)
		}
	}
	if r := Decide(it, DefaultCaps(), JA, nil, "").Reason; r != "video hevc yuv420p10le not playable" {
		t.Error(r)
	}
}

func TestForceStepsDown(t *testing.T) {
	it := item("/m/x.mp4", "mov,mp4", vd(), a(1, aopt{}))
	if Decide(it, DefaultCaps(), JA, nil, "remux").Mode != "remux" {
		t.Error("remux")
	}
	d := Decide(it, DefaultCaps(), JA, nil, "transcode")
	if d.Mode != "transcode" || d.Video != "transcode" || d.Audio != "aac" {
		t.Error(d)
	}
}

func TestExplicitAudioChoice(t *testing.T) {
	it := item("", "", vd(), a(1, aopt{lang: "jpn"}), a(2, aopt{lang: "eng"}))
	if *Decide(it, DefaultCaps(), JA, ip(2), "").AudioIndex != 2 || *Decide(it, DefaultCaps(), JA, ip(99), "").AudioIndex != 1 {
		t.Error("explicit audio")
	}
}

func TestFFmpegArgsNeverMapSubtitles(t *testing.T) {
	it := item("", "", v("hevc", "yuv420p", "Main", 0, 1080), a(1, aopt{lang: "eng"}), a(2, aopt{lang: "jpn", codec: "eac3", channels: 6}))
	d := Decide(it, ParseCaps("hevc", ""), JA, nil, "")
	args := FFmpegArgs(cfg(), it, d, 12.5)
	joined := strings.Join(args, " ")
	if !has(args, "-sn") || strings.Contains(joined, "0:s") {
		t.Error("subtitles")
	}
	if argAfter(args, "-ss") != "12.500" || index(args, "-ss") > index(args, "-i") {
		t.Error("seek")
	}
	if !reflect.DeepEqual(maps(args), []string{"0:0", "0:2"}) {
		t.Error(maps(args))
	}
	if !has(args, "-tag:v") || !has(args, "hvc1") || argAfter(args, "-ac") != "2" {
		t.Error(args)
	}
	if args[len(args)-1] != "pipe:1" || !strings.Contains(joined, "frag_keyframe") {
		t.Error(args)
	}
}

func TestFFmpegTranscode(t *testing.T) {
	big := item("", "", v("mpeg2video", "yuv420p", "High", 0, 2160), a(1, aopt{}))
	if vf := argAfter(FFmpegArgs(cfg(), big, Decide(big, DefaultCaps(), JA, nil, ""), 0), "-vf"); !strings.Contains(vf, "scale=-2:1080") {
		t.Error(vf)
	}
	small := item("", "", v("mpeg2video", "yuv420p", "High", 0, 480), a(1, aopt{}))
	if vf := argAfter(FFmpegArgs(cfg(), small, Decide(small, DefaultCaps(), JA, nil, ""), 0), "-vf"); strings.Contains(vf, "scale") {
		t.Error(vf)
	}
	h := item("", "", v("hevc", "yuv420p10le", "High", 0, 1080, hdr), a(1, aopt{}))
	if vf := argAfter(FFmpegArgs(cfg(), h, Decide(h, DefaultCaps(), JA, nil, ""), 0), "-vf"); !strings.Contains(vf, "tonemap") {
		t.Error(vf)
	}
	va := item("", "", v("hevc", "yuv420p10le", "High", 0, 2160), a(1, aopt{}))
	args := FFmpegArgs(cfg(func(c *config.Config) { c.HWAccel = "vaapi" }), va, Decide(va, DefaultCaps(), JA, nil, ""), 5)
	if index(args, "-hwaccel") > index(args, "-i") || !has(args, "h264_vaapi") || argAfter(args, "-vf") != "scale_vaapi=w=-2:h=1080:format=nv12" {
		t.Error(args)
	}
	cp := item("", "", vd(), a(1, aopt{}))
	args = FFmpegArgs(cfg(), cp, Decide(cp, DefaultCaps(), JA, nil, ""), 0)
	if argAfter(args, "-c:v") != "copy" || has(args, "libx264") {
		t.Error(args)
	}
}

func TestX264ThreadsAndLookaheadAreCapped(t *testing.T) {
	it := item("", "", v("hevc", "yuv420p", "High", 0, 1080), a(1, aopt{}))
	args := FFmpegArgs(cfg(), it, Decide(it, DefaultCaps(), JA, nil, ""), 0)
	if argAfter(args, "-threads") != "4" || argAfter(args, "-rc-lookahead") != "10" {
		t.Error(args)
	}
	args = FFmpegArgs(cfg(func(c *config.Config) { c.TranscodeThreads = 0 }), it, Decide(it, DefaultCaps(), JA, nil, ""), 0)
	if has(args, "-threads") {
		t.Error(args)
	}
}

func TestConvertCommand(t *testing.T) {
	it := item("/m/Show/It's E01.mkv", "", a(0, aopt{codec: "flac"}), v("hevc", "yuv420p10le", "High", 1, 1080), a(2, aopt{}))
	d := Decide(it, DefaultCaps(), JA, nil, "")
	if d.Mode != "transcode" {
		t.Fatal(d)
	}
	want := `ffmpeg -hide_banner -i 'It'"'"'s E01.mkv' -map 0:1 -map '0:a?' -map '0:s?' -map '0:t?' -c copy ` +
		`-c:v libx264 -preset slow -crf 18 -profile:v high -vf format=yuv420p -f matroska ` +
		`'It'"'"'s E01.mkv.part' && mv -- 'It'"'"'s E01.mkv.part' 'It'"'"'s E01.mkv'`
	if got := ConvertCommand(it, d); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	h := item("/m/x.mp4", "mov,mp4", v("hevc", "yuv420p10le", "High", 0, 1080, hdr), a(1, aopt{}))
	cmd := ConvertCommand(h, Decide(h, DefaultCaps(), JA, nil, ""))
	if !strings.Contains(cmd, "tonemap=") || strings.Contains(cmd, "0:s?") || !strings.HasSuffix(cmd, "mv -- x.mkv.part x.mkv") {
		t.Error(cmd)
	}
}

func TestConvertedCopy(t *testing.T) {
	it := item("", "", v("hevc", "yuv420p10le", "High", 0, 2160), a(1, aopt{codec: "ac3", lang: "eng", channels: 6}),
		a(2, aopt{codec: "flac", lang: "jpn", channels: 6}), a(3, aopt{codec: "aac", lang: "fra"}))
	args, _ := ConvertedArgs(cfg(), it, JA, "/c/x.mp4.part")
	if !reflect.DeepEqual(maps(args), []string{"0:0", "0:2"}) || !has(args, "-sn") || !has(args, "libx264") {
		t.Error(args)
	}
	// Same picture as the source: no MAX_HEIGHT scaling, not the live VIDEO_CRF.
	if argAfter(args, "-vf") != "format=yuv420p" || argAfter(args, "-crf") != "18" {
		t.Error(args)
	}
	// The track a browser plays must be AAC.
	if argAfter(args, "-c:a") != "aac" || argAfter(args, "-ac") != "2" || argAfter(args, "-disposition:a:0") != "default" {
		t.Error(args)
	}
	if !has(args, "+faststart") || !reflect.DeepEqual(args[len(args)-3:], []string{"-f", "mp4", "/c/x.mp4.part"}) || has(args, "-map_chapters") {
		t.Error(args)
	}

	engFra := item("", "", v("hevc", "yuv420p", "High", 0, 1080), a(1, aopt{lang: "fra", def: true}), a(2, aopt{lang: "eng"}))
	args, _ = ConvertedArgs(cfg(), engFra, JA, "/c/x.mp4.part")
	if !reflect.DeepEqual(maps(args), []string{"0:0", "0:2"}) || ConvertedAudio(engFra.Streams, JA).Index != 2 {
		t.Error(args)
	}
	other := item("", "", v("hevc", "yuv420p", "High", 0, 1080), a(1, aopt{lang: "ger"}), a(2, aopt{lang: "fra", def: true}))
	if ConvertedAudio(other.Streams, JA).Index != 2 {
		t.Error("default")
	}
	silent := item("", "", v("hevc", "yuv420p", "High", 0, 1080))
	if args, _ := ConvertedArgs(cfg(), silent, JA, "/c/x.mp4.part"); !has(args, "-an") {
		t.Error(args)
	}
	h := item("", "", v("hevc", "yuv420p10le", "High", 0, 1080, hdr), a(1, aopt{}))
	args, _ = ConvertedArgs(cfg(), h, JA, "/c/x.mp4.part")
	if !strings.Contains(argAfter(args, "-vf"), "tonemap=") || argAfter(args, "-c:a") != "copy" {
		t.Error(args)
	}
	if _, err := ConvertedArgs(cfg(), item("", "", a(0, aopt{})), JA, "x"); err == nil {
		t.Error("no video should fail")
	}
}

func TestStreamMIME(t *testing.T) {
	it := item("", "", v("h264", "yuv420p", "Main", 0, 1080), a(1, aopt{codec: "ac3", channels: 6}))
	if m := StreamMIME(cfg(), it, Decide(it, DefaultCaps(), JA, nil, "")); m != `video/mp4; codecs="avc1.4D4028,mp4a.40.2"` {
		t.Error(m)
	}
	if m := StreamMIME(cfg(), it, Decide(it, ParseCaps("", "ac3"), JA, nil, "")); m != `video/mp4; codecs="avc1.4D4028,ac-3"` {
		t.Error(m)
	}
	h := item("", "", v("hevc", "yuv420p10le", "Main 10", 0, 2160), a(1, aopt{}))
	if m := StreamMIME(cfg(), h, Decide(h, ParseCaps("hevc,hevc10", ""), JA, nil, "")); m != `video/mp4; codecs="hvc1.2.4.L153.90,mp4a.40.2"` {
		t.Error(m)
	}
	d := Decide(h, DefaultCaps(), JA, nil, "")
	if m := StreamMIME(cfg(), h, d); m != `video/mp4; codecs="avc1.640028,mp4a.40.2"` {
		t.Error(m)
	}
	if m := StreamMIME(cfg(func(c *config.Config) { c.MaxHeight = 0 }), h, d); m != `video/mp4; codecs="avc1.640033,mp4a.40.2"` {
		t.Error(m)
	}
	n := item("", "", vd())
	if m := StreamMIME(cfg(), n, Decide(n, DefaultCaps(), JA, nil, "")); m != `video/mp4; codecs="avc1.640028"` {
		t.Error(m)
	}
}

func TestDescribeAudio(t *testing.T) {
	got := DescribeAudio([]probe.Stream{vd(), a(1, aopt{lang: "jpn", title: "Stereo"}), {Index: 2, Type: "audio", Codec: "ac3"}})
	want := []AudioTrack{{1, "JPN · Stereo · aac 2ch", "jpn"}, {2, "ac3 ?ch", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Error(got)
	}
}
