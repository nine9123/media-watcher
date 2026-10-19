package store

// EpisodeLess is play order: seasons in order with specials (season 0)
// last, numbered episodes before unnumbered ones, then by file.
func EpisodeLess(a, b *Item) bool {
	sa, sb := seasonKey(a.Season), seasonKey(b.Season)
	if sa != sb {
		return sa < sb
	}
	ea, eb := a.Episode == nil, b.Episode == nil
	if ea != eb {
		return !ea
	}
	if a.Episode != nil && *a.Episode != *b.Episode {
		return *a.Episode < *b.Episode
	}
	if a.SortName != b.SortName {
		return a.SortName < b.SortName
	}
	return a.ID < b.ID
}

// seasonKey sorts like SQLite's "ORDER BY season=0, season": NULL first,
// then 1, 2, ..., then 0.
func seasonKey(s *int) int64 {
	switch {
	case s == nil:
		return -1
	case *s == 0:
		return 1 << 40
	}
	return int64(*s)
}
