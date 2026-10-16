from mediawatcher import probe


def ch(start, end, title):
    return {"start_time": str(start), "end_time": str(end), "tags": {"title": title}}


def test_skips_from_opening_and_ending_chapters():
    chapters = [ch(0, 90, "Prologue"), ch(90, 180, "Opening"), ch(180, 1200, "Part A"),
                ch(1200, 1290, "Ending"), ch(1290, 1320, "Preview")]
    assert probe.skips(chapters) == [
        {"kind": "intro", "start": 90.0, "end": 180.0},
        {"kind": "credits", "start": 1200.0, "end": 1290.0},
    ]


def test_skip_names_are_matched_whole_and_loosely():
    assert [s["kind"] for s in probe.skips([ch(0, 89, "OP"), ch(1300, 1389, "ED 2")])] == \
        ["intro", "credits"]
    assert [s["kind"] for s in probe.skips([ch(5, 95, "Opening Credits")])] == ["intro"]
    assert [s["kind"] for s in probe.skips([ch(5, 95, "Op.")])] == ["intro"]
    # A story chapter that merely starts with the word is not an opening.
    assert probe.skips([ch(0, 120, "Opening Scene"), ch(1300, 1400, "The End")]) == []


def test_skips_ignore_implausible_lengths_and_follow_start_time():
    assert probe.skips([ch(0, 2, "Intro"), ch(0, 1400, "Credits")]) == []
    assert probe.skips([ch(11.5, 101.5, "Intro")], start_time=1.5) == \
        [{"kind": "intro", "start": 10.0, "end": 100.0}]


def test_summarize_keeps_skips():
    raw = {"format": {"duration": "1440", "start_time": "0"}, "streams": [],
           "chapters": [ch(60, 150, "Opening")]}
    assert probe.summarize(raw)["skips"] == [{"kind": "intro", "start": 60.0, "end": 150.0}]
    assert probe.summarize({"format": {}})["skips"] == []
