// Package probe wraps ffprobe and keeps only what playback needs.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const entries = "format=duration,start_time,format_name" +
	":stream=index,codec_type,codec_name,profile,width,height,pix_fmt,color_transfer," +
	"channels,channel_layout:stream_tags:stream_disposition" +
	":chapter=start_time,end_time:chapter_tags=title"

// Chapter names that BD/anime encodes use for the opening and ending songs.
// Matched against the whole (normalised) title, so "Opening Scene" or
// "The End" are left alone.
var (
	introRE = regexp.MustCompile(`^(?:op|opening|intro|introduction|op \d+|opening (theme|song|credits)|` +
		`title sequence|main titles?)$`)
	creditsRE = regexp.MustCompile(`^(?:ed|ending|outro|credits|end credits|closing credits|ed \d+|` +
		`ending (theme|song|credits))$`)
	nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
)

const skipMin, skipMax = 5.0, 360.0 // anything outside this is not an OP/ED

// Stream is one video or audio stream of a file.
type Stream struct {
	Index         int     `json:"index"`
	Type          string  `json:"type"`
	Codec         string  `json:"codec"`
	Profile       *string `json:"profile"`
	Language      string  `json:"language"`
	Title         string  `json:"title"`
	Default       bool    `json:"default"`
	Comment       bool    `json:"comment"`
	Width         *int    `json:"width,omitempty"`
	Height        *int    `json:"height,omitempty"`
	PixFmt        *string `json:"pix_fmt,omitempty"`
	ColorTransfer *string `json:"color_transfer,omitempty"`
	Channels      *int    `json:"channels,omitempty"`
	Layout        *string `json:"layout,omitempty"`
}

func (s Stream) ProfileString() string {
	if s.Profile == nil {
		return ""
	}
	return *s.Profile
}

func (s Stream) PixFmtString() string {
	if s.PixFmt == nil {
		return ""
	}
	return *s.PixFmt
}

func (s Stream) HeightInt() int {
	if s.Height == nil {
		return 0
	}
	return *s.Height
}

func (s Stream) ChannelsInt() int {
	if s.Channels == nil {
		return 0
	}
	return *s.Channels
}

// Skip is an intro or credits range in seconds from the start of the file.
type Skip struct {
	Kind  string  `json:"kind"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Info is the part of ffprobe's output that is stored per item.
type Info struct {
	Duration   *float64
	StartTime  float64
	Container  string
	VideoCodec *string
	Width      *int
	Height     *int
	PixFmt     *string
	Streams    []Stream
	Skips      []Skip
}

// Raw is ffprobe's JSON, the parts that are read.
type Raw struct {
	Format struct {
		Duration   *string `json:"duration"`
		StartTime  *string `json:"start_time"`
		FormatName string  `json:"format_name"`
	} `json:"format"`
	Streams []struct {
		Index         int               `json:"index"`
		CodecType     string            `json:"codec_type"`
		CodecName     string            `json:"codec_name"`
		Profile       *string           `json:"profile"`
		Width         *int              `json:"width"`
		Height        *int              `json:"height"`
		PixFmt        *string           `json:"pix_fmt"`
		ColorTransfer *string           `json:"color_transfer"`
		Channels      *int              `json:"channels"`
		ChannelLayout *string           `json:"channel_layout"`
		Tags          map[string]string `json:"tags"`
		Disposition   map[string]int    `json:"disposition"`
	} `json:"streams"`
	Chapters []Chapter `json:"chapters"`
}

type Chapter struct {
	StartTime *string           `json:"start_time"`
	EndTime   *string           `json:"end_time"`
	Tags      map[string]string `json:"tags"`
}

// Probe runs ffprobe on path. ctx cancels it (shutdown).
func Probe(ctx context.Context, ffprobe, path string) (*Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// Only what Summarize reads: anime MKVs carry dozens of font
	// attachments whose full stream dumps would be parsed for nothing.
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-hide_banner",
		"-print_format", "json", "-show_entries", entries, path)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		if msg == "" {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				msg = "ffprobe failed"
			} else {
				msg = err.Error()
			}
		}
		return nil, errors.New(msg)
	}
	var raw Raw
	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		out = "{}"
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("ffprobe output: %w", err)
	}
	return Summarize(&raw), nil
}

func parseFloat(v *string) *float64 {
	if v == nil {
		return nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(*v), 64)
	if err != nil {
		return nil
	}
	return &f
}

// Summarize reduces ffprobe output to the fields that are stored.
func Summarize(raw *Raw) *Info {
	info := &Info{Container: raw.Format.FormatName, Streams: []Stream{}}
	var video *Stream
	for _, s := range raw.Streams {
		if s.CodecType != "video" && s.CodecType != "audio" {
			continue // subtitles are never shown; attachments/data are useless here
		}
		if s.CodecType == "video" && s.Disposition["attached_pic"] != 0 {
			continue // cover art embedded as a video stream
		}
		tags := map[string]string{}
		for k, v := range s.Tags {
			tags[strings.ToLower(k)] = v
		}
		e := Stream{
			Index: s.Index, Type: s.CodecType, Codec: strings.ToLower(s.CodecName), Profile: s.Profile,
			Language: strings.ToLower(tags["language"]), Title: tags["title"],
			Default: s.Disposition["default"] != 0,
			Comment: s.Disposition["comment"] != 0 || strings.Contains(strings.ToLower(tags["title"]), "commentary"),
		}
		if s.CodecType == "video" {
			e.Width, e.Height, e.PixFmt, e.ColorTransfer = s.Width, s.Height, s.PixFmt, s.ColorTransfer
		} else {
			e.Channels, e.Layout = s.Channels, s.ChannelLayout
		}
		info.Streams = append(info.Streams, e)
		if s.CodecType == "video" && video == nil {
			video = &info.Streams[len(info.Streams)-1]
		}
	}
	if st := parseFloat(raw.Format.StartTime); st != nil {
		info.StartTime = *st
	}
	info.Duration = parseFloat(raw.Format.Duration)
	if video != nil {
		c := video.Codec
		info.VideoCodec, info.Width, info.Height, info.PixFmt = &c, video.Width, video.Height, video.PixFmt
	}
	info.Skips = Skips(raw.Chapters, info.StartTime)
	return info
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// Skips are intro/credits ranges from chapter names, in seconds from the file start.
//
// At most one of each kind: the first opening and the last ending chapter.
func Skips(chapters []Chapter, startTime float64) []Skip {
	var intro, credits *Skip
	for _, ch := range chapters {
		name := strings.Join(strings.Fields(nonAlnum.ReplaceAllString(strings.ToLower(ch.Tags["title"]), " ")), " ")
		start, end := parseFloat(ch.StartTime), parseFloat(ch.EndTime)
		if start == nil || end == nil || *end-*start < skipMin || *end-*start > skipMax {
			continue
		}
		rng := Skip{Start: round3(max(0, *start-startTime)), End: round3(max(0, *end-startTime))}
		if intro == nil && introRE.MatchString(name) {
			rng.Kind = "intro"
			intro = &rng
		} else if creditsRE.MatchString(name) {
			rng.Kind = "credits"
			credits = &rng
		}
	}
	out := []Skip{}
	for _, s := range []*Skip{intro, credits} {
		if s != nil {
			out = append(out, *s)
		}
	}
	return out
}

// KeyframeBefore is the time (relative to file start) of the last video
// keyframe at or before t, or nil.
//
// Used when the video stream is copied: ffmpeg can only start a copied
// stream on a keyframe, so we start exactly there and tell the player the
// real start time instead of letting the clock drift by up to a GOP.
func KeyframeBefore(ctx context.Context, ffprobe, path string, t, startTime float64) *float64 {
	if t <= 0 {
		z := 0.0
		return &z
	}
	abs := t + startTime
	lo := max(0, abs-30)
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-skip_frame", "nokey", "-show_entries", "frame=pts_time,best_effort_timestamp_time",
		"-of", "csv=p=0", "-read_intervals", fmt.Sprintf("%.3f%%%.3f", lo, abs+0.05), path).Output()
	if ctx.Err() != nil {
		return nil
	}
	var best *float64
	for _, line := range strings.Split(string(out), "\n") {
		for _, cell := range strings.Split(line, ",") {
			cell = strings.TrimSpace(cell)
			v := parseFloat(&cell)
			if v != nil && *v <= abs+0.05 && (best == nil || *v > *best) {
				best = v
			}
		}
	}
	if best == nil {
		return nil
	}
	r := max(0, *best-startTime)
	return &r
}
