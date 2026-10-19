package probe

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
)

func ch(start, end float64, title string) Chapter {
	s, e := strconv.FormatFloat(start, 'f', -1, 64), strconv.FormatFloat(end, 'f', -1, 64)
	return Chapter{StartTime: &s, EndTime: &e, Tags: map[string]string{"title": title}}
}

func kinds(s []Skip) []string {
	out := []string{}
	for _, x := range s {
		out = append(out, x.Kind)
	}
	return out
}

func TestSkipsFromOpeningAndEndingChapters(t *testing.T) {
	got := Skips([]Chapter{ch(0, 90, "Prologue"), ch(90, 180, "Opening"), ch(180, 1200, "Part A"),
		ch(1200, 1290, "Ending"), ch(1290, 1320, "Preview")}, 0)
	want := []Skip{{"intro", 90, 180}, {"credits", 1200, 1290}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestSkipNamesAreMatchedWholeAndLoosely(t *testing.T) {
	if k := kinds(Skips([]Chapter{ch(0, 89, "OP"), ch(1300, 1389, "ED 2")}, 0)); !reflect.DeepEqual(k, []string{"intro", "credits"}) {
		t.Error(k)
	}
	if k := kinds(Skips([]Chapter{ch(5, 95, "Opening Credits")}, 0)); !reflect.DeepEqual(k, []string{"intro"}) {
		t.Error(k)
	}
	if k := kinds(Skips([]Chapter{ch(5, 95, "Op.")}, 0)); !reflect.DeepEqual(k, []string{"intro"}) {
		t.Error(k)
	}
	// A story chapter that merely starts with the word is not an opening.
	if k := Skips([]Chapter{ch(0, 120, "Opening Scene"), ch(1300, 1400, "The End")}, 0); len(k) != 0 {
		t.Error(k)
	}
}

func TestSkipsIgnoreImplausibleLengthsAndFollowStartTime(t *testing.T) {
	if k := Skips([]Chapter{ch(0, 2, "Intro"), ch(0, 1400, "Credits")}, 0); len(k) != 0 {
		t.Error(k)
	}
	got := Skips([]Chapter{ch(11.5, 101.5, "Intro")}, 1.5)
	if !reflect.DeepEqual(got, []Skip{{"intro", 10, 100}}) {
		t.Error(got)
	}
}

func TestSummarizeKeepsSkipsAndStreams(t *testing.T) {
	var raw Raw
	data := `{"format": {"duration": "1440", "start_time": "0", "format_name": "matroska,webm"},
	  "streams": [
	    {"index": 0, "codec_type": "video", "codec_name": "H264", "profile": "High", "width": 1920,
	     "height": 1080, "pix_fmt": "yuv420p", "disposition": {"default": 1}},
	    {"index": 1, "codec_type": "video", "codec_name": "mjpeg", "disposition": {"attached_pic": 1}},
	    {"index": 2, "codec_type": "audio", "codec_name": "aac", "channels": 2,
	     "tags": {"LANGUAGE": "JPN", "title": "Commentary"}},
	    {"index": 3, "codec_type": "subtitle", "codec_name": "ass"}],
	  "chapters": [{"start_time": "60", "end_time": "150", "tags": {"title": "Opening"}}]}`
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		t.Fatal(err)
	}
	info := Summarize(&raw)
	if !reflect.DeepEqual(info.Skips, []Skip{{"intro", 60, 150}}) {
		t.Error(info.Skips)
	}
	if len(info.Streams) != 2 || info.Streams[0].Codec != "h264" || *info.VideoCodec != "h264" || *info.Height != 1080 {
		t.Errorf("%+v", info.Streams)
	}
	a := info.Streams[1]
	if a.Language != "jpn" || !a.Comment || a.Default || a.ChannelsInt() != 2 || a.Height != nil {
		t.Errorf("%+v", a)
	}
	if info.Container != "matroska,webm" || *info.Duration != 1440 {
		t.Errorf("%+v", info)
	}
	if s := Summarize(&Raw{}).Skips; len(s) != 0 {
		t.Error(s)
	}
}
