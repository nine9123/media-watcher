import shlex

from mediawatcher.config import Config
from mediawatcher.playback import (Caps, convert_command, decide, ffmpeg_args, pick_audio,
                                  stream_mime)

JA = ["jpn", "ja", "japanese"]


def v(codec="h264", pix="yuv420p", profile="High", index=0, height=1080, **kw):
    return {"index": index, "type": "video", "codec": codec, "pix_fmt": pix, "profile": profile,
            "height": height, "width": height * 16 // 9, **kw}


def a(index, codec="aac", lang="", title="", default=False, comment=False, channels=2):
    return {"index": index, "type": "audio", "codec": codec, "language": lang, "title": title,
            "default": default, "comment": comment, "channels": channels}


def item(path="/m/x.mkv", container="matroska,webm", streams=()):
    return {"path": path, "container": container, "streams": list(streams)}


def cfg(**kw):
    return Config(api_key="x" * 8, libraries=[], **kw)


def test_japanese_wins_over_default_english():
    s = [v(), a(1, lang="eng", default=True), a(2, lang="jpn")]
    assert pick_audio(s, JA)["index"] == 2


def test_japanese_by_title_when_untagged():
    s = [v(), a(1, title="English 5.1", default=True), a(2, title="Japanese 2.0")]
    assert pick_audio(s, JA)["index"] == 2


def test_japanese_commentary_is_last_resort():
    s = [v(), a(1, lang="jpn", title="Commentary", comment=True), a(2, lang="jpn")]
    assert pick_audio(s, JA)["index"] == 2


def test_no_japanese_falls_back_to_default_then_first():
    assert pick_audio([v(), a(1, lang="eng"), a(2, lang="ger", default=True)], JA)["index"] == 2
    assert pick_audio([v(), a(1, lang="eng"), a(2, lang="ger")], JA)["index"] == 1
    assert pick_audio([v()], JA) is None


def test_direct_play_mp4():
    it = item("/m/x.mp4", "mov,mp4,m4a,3gp,3g2,mj2", [v(), a(1, lang="jpn")])
    assert decide(it, Caps(), JA).mode == "direct"


def test_mp4_with_japanese_second_track_is_remuxed():
    it = item("/m/x.mp4", "mov,mp4,m4a,3gp,3g2,mj2", [v(), a(1, lang="eng"), a(2, lang="jpn")])
    d = decide(it, Caps(), JA)
    assert (d.mode, d.audio_index, d.video, d.audio) == ("remux", 2, "copy", "copy")


def test_mkv_h264_ac3_remux_audio_to_aac():
    d = decide(item(streams=[v(), a(1, codec="ac3", channels=6)]), Caps(), JA)
    assert (d.mode, d.video, d.audio) == ("remux", "copy", "aac")
    d = decide(item(streams=[v(), a(1, codec="ac3")]), Caps.parse("", "ac3"), JA)
    assert d.audio == "copy"


def test_hevc_depends_on_browser():
    s = [v("hevc", "yuv420p10le", "Main 10"), a(1, codec="flac")]
    assert decide(item(streams=s), Caps(), JA).mode == "transcode"
    assert decide(item(streams=s), Caps.parse("hevc", ""), JA).mode == "transcode"
    d = decide(item(streams=s), Caps.parse("hevc,hevc10", "flac"), JA)
    assert (d.mode, d.video, d.audio) == ("remux", "copy", "copy")


def test_hi10p_h264_is_transcoded():
    d = decide(item(streams=[v(pix="yuv420p10le", profile="High 10"), a(1)]), Caps(), JA)
    assert d.mode == "transcode"


def test_old_codecs_transcode():
    for codec in ("mpeg2video", "vc1", "mpeg4", "msmpeg4v3"):
        assert decide(item(streams=[v(codec), a(1)]), Caps(), JA).mode == "transcode"


def test_force_steps_down():
    it = item("/m/x.mp4", "mov,mp4", [v(), a(1)])
    assert decide(it, Caps(), JA, force="remux").mode == "remux"
    d = decide(it, Caps(), JA, force="transcode")
    assert (d.mode, d.video, d.audio) == ("transcode", "transcode", "aac")


def test_explicit_audio_choice():
    s = [v(), a(1, lang="jpn"), a(2, lang="eng")]
    assert decide(item(streams=s), Caps(), JA, audio_index=2).audio_index == 2
    assert decide(item(streams=s), Caps(), JA, audio_index=99).audio_index == 1


def test_ffmpeg_args_never_map_subtitles():
    it = item(streams=[v("hevc"), a(1, lang="eng"), a(2, lang="jpn", codec="eac3", channels=6)])
    d = decide(it, Caps.parse("hevc", ""), JA)
    args = ffmpeg_args(cfg(), it, d, start=12.5)
    joined = " ".join(args)
    assert "-sn" in args and "0:s" not in joined
    assert args[args.index("-ss") + 1] == "12.500"
    assert args.index("-ss") < args.index("-i")  # fast input seek
    first_map = args.index("-map")
    assert args[first_map:first_map + 4] == ["-map", "0:0", "-map", "0:2"]
    assert "-tag:v" in args and "hvc1" in args
    assert args[args.index("-ac") + 1] == "2"
    assert args[-1] == "pipe:1" and "frag_keyframe" in joined


def test_ffmpeg_transcode_scales_only_when_needed():
    big = item(streams=[v("mpeg2video", height=2160), a(1)])
    args = ffmpeg_args(cfg(), big, decide(big, Caps(), JA))
    assert "scale=-2:1080" in args[args.index("-vf") + 1]
    small = item(streams=[v("mpeg2video", height=480), a(1)])
    args = ffmpeg_args(cfg(), small, decide(small, Caps(), JA))
    assert "scale" not in args[args.index("-vf") + 1]


def test_ffmpeg_hdr_tonemaps_in_software():
    it = item(streams=[v("hevc", "yuv420p10le", color_transfer="smpte2084"), a(1)])
    args = ffmpeg_args(cfg(), it, decide(it, Caps(), JA))
    assert "tonemap" in args[args.index("-vf") + 1]


def test_ffmpeg_vaapi():
    it = item(streams=[v("hevc", "yuv420p10le", height=2160), a(1)])
    args = ffmpeg_args(cfg(hwaccel="vaapi"), it, decide(it, Caps(), JA), start=5)
    assert args.index("-hwaccel") < args.index("-i")
    assert "h264_vaapi" in args
    assert args[args.index("-vf") + 1] == "scale_vaapi=w=-2:h=1080:format=nv12"


def test_copy_mode_has_no_video_encoder():
    it = item(streams=[v(), a(1)])
    args = ffmpeg_args(cfg(), it, decide(it, Caps(), JA))
    assert args[args.index("-c:v") + 1] == "copy"
    assert "libx264" not in args


def test_x264_threads_and_lookahead_are_capped():
    it = item(streams=[v(codec="hevc"), a(1)])
    args = ffmpeg_args(cfg(), it, decide(it, Caps(), JA))
    assert args[args.index("-threads") + 1] == "4"
    assert args[args.index("-rc-lookahead") + 1] == "10"
    args = ffmpeg_args(cfg(transcode_threads=0), it, decide(it, Caps(), JA))
    assert "-threads" not in args


def test_convert_command_keeps_tracks_and_replaces_mkv_in_place():
    it = item(path="/m/Show/It's E01.mkv",
              streams=[a(0, codec="flac"), v("hevc", pix="yuv420p10le", index=1), a(2)])
    d = decide(it, Caps(), JA)
    assert d.mode == "transcode"
    enc, mv = convert_command(it, d).split(" && ")
    args = shlex.split(enc)
    assert args[args.index("-i") + 1] == "It's E01.mkv"
    assert [args[i + 1] for i, x in enumerate(args) if x == "-map"] == ["0:1", "0:a?", "0:s?", "0:t?"]
    assert "libx264" in args and "format=yuv420p" in args and "zscale" not in enc
    assert args[-1] == "It's E01.mkv.part"
    assert shlex.split(mv) == ["mv", "--", "It's E01.mkv.part", "It's E01.mkv"]


def test_convert_command_tonemaps_hdr_and_renames_other_containers():
    it = item(path="/m/x.mp4", container="mov,mp4",
              streams=[v("hevc", pix="yuv420p10le", color_transfer="smpte2084"), a(1)])
    cmd = convert_command(it, decide(it, Caps(), JA))
    assert "tonemap=" in cmd and "0:s?" not in cmd
    assert cmd.endswith("mv -- x.mkv.part x.mkv")


def test_stream_mime_names_the_codecs_ffmpeg_writes():
    it = item(streams=[v(profile="Main"), a(1, codec="ac3", channels=6)])
    d = decide(it, Caps(), JA)
    assert stream_mime(cfg(), it, d) == 'video/mp4; codecs="avc1.4D4028,mp4a.40.2"'
    d = decide(it, Caps.parse("", "ac3"), JA)
    assert stream_mime(cfg(), it, d) == 'video/mp4; codecs="avc1.4D4028,ac-3"'


def test_stream_mime_for_hevc_copy_and_transcode():
    it = item(streams=[v("hevc", "yuv420p10le", "Main 10", height=2160), a(1)])
    d = decide(it, Caps.parse("hevc,hevc10", ""), JA)
    assert stream_mime(cfg(), it, d) == 'video/mp4; codecs="hvc1.2.4.L153.90,mp4a.40.2"'
    d = decide(it, Caps(), JA)
    assert d.video == "transcode"
    assert stream_mime(cfg(max_height=1080), it, d) == 'video/mp4; codecs="avc1.640028,mp4a.40.2"'
    assert stream_mime(cfg(max_height=0), it, d) == 'video/mp4; codecs="avc1.640033,mp4a.40.2"'


def test_stream_mime_without_audio():
    it = item(streams=[v()])
    assert stream_mime(cfg(), it, decide(it, Caps(), JA)) == 'video/mp4; codecs="avc1.640028"'
