package scan

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWalkFollowsSymlinksButNotLoops(t *testing.T) {
	dir := t.TempDir()
	root, elsewhere := filepath.Join(dir, "Shows"), filepath.Join(dir, "elsewhere")
	for _, p := range []string{filepath.Join(root, "Show", "Season 01"), filepath.Join(elsewhere, "Linked")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "Show", "Season 01", "Show - S01E01.mkv"), nil, 0o644)
	os.WriteFile(filepath.Join(elsewhere, "Linked", "Linked - S01E01.mkv"), nil, 0o644)
	for link, target := range map[string]string{
		filepath.Join(root, "Show", "Season 01", "up"): "..",                  // a loop
		filepath.Join(root, "Show", "root"):            root,                  // a loop
		filepath.Join(root, "Linked"):                  elsewhere + "/Linked", // followed
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	var videos []string
	for _, d := range walkLibrary(root) {
		for _, v := range d.videos {
			videos = append(videos, filepath.Join(d.rel, v))
		}
	}
	want := []string{"Show/Season 01/Show - S01E01.mkv", "Linked/Linked - S01E01.mkv"}
	if len(videos) != 2 || !(videos[0] == want[0] && videos[1] == want[1] || videos[0] == want[1] && videos[1] == want[0]) {
		t.Errorf("%q", videos)
	}
}
