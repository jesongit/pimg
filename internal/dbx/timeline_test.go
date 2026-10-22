package dbx

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func TestTimelineScopeAndSeekAcrossAllPages(t *testing.T) {
	d := telegramTestDB(t)
	zone := time.FixedZone("test", 8*3600)
	stamp := func(s string) int64 {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, zone)
		if err != nil {
			t.Fatal(err)
		}
		return v.Unix()
	}
	for i, item := range []struct {
		id, date, kind, name string
		hidden               bool
	}{
		{"old", "2023-01-01 00:00", "image", "match", false},
		{"last", "2026-09-30 23:59", "image", "match", false},
		{"first", "2026-10-01 00:00", "image", "match", false},
		{"video", "2024-05-01 12:00", "video", "match", false},
		{"other", "2022-05-01 12:00", "image", "other", false},
		{"hidden", "2021-05-01 12:00", "image", "match", true},
	} {
		f := telegramTestFile(item.id, 0)
		if err := d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE files SET created_at=?,kind=?,name=?,hidden=?,published=?,archive_state='done' WHERE id=?`, stamp(item.date), item.kind, item.name, item.hidden, i%2, item.id); err != nil {
			t.Fatal(err)
		}
	}
	filter := FileFilter{Query: "match", Kind: "image", PerPage: 1}
	months, err := d.FileTimeline(filter, 480)
	if err != nil {
		t.Fatal(err)
	}
	if len(months) != 3 || months[0].Key != "2026-10" || months[1].Key != "2026-09" || months[2].Key != "2023-01" {
		t.Fatalf("wrong scope/boundaries: %+v", months)
	}
	filter.Before = months[1].End
	files, total, err := d.ListFiles(filter)
	if err != nil || total != 2 || len(files) != 1 || files[0].ID != "last" {
		t.Fatalf("seek beyond first page: %v %d %+v", err, total, files)
	}
	filter.Page = 2
	files, _, err = d.ListFiles(filter)
	if err != nil || len(files) != 1 || files[0].ID != "old" {
		t.Fatalf("seek pagination: %v %+v", err, files)
	}
	unchanged, err := d.FileTimeline(filter, 480)
	if err != nil || len(unchanged) != 3 {
		t.Fatalf("seek shrank global index: %v %+v", err, unchanged)
	}
	filter.Before = 0
	filter.After = months[1].Start
	filter.Page = 1
	filter.Sort = "oldest"
	files, total, err = d.ListFiles(filter)
	if err != nil || total != 2 || files[0].ID != "last" {
		t.Fatalf("ascending seek: %v %d %+v", err, total, files)
	}
	filter.State = "published"
	months, err = d.FileTimeline(filter, 480)
	if err != nil || len(months) != 1 || months[0].Key != "2026-09" {
		t.Fatalf("published scope: %v %+v", err, months)
	}
	if _, err = d.FileTimeline(filter, 841); err == nil {
		t.Fatal("invalid timezone accepted")
	}
}

func TestTimelineGroupsCalendarBoundsAndFullScope(t *testing.T) {
	d := telegramTestDB(t)
	album, err := d.InsertAlbum("Scoped dates")
	if err != nil {
		t.Fatal(err)
	}
	otherAlbum, err := d.InsertAlbum("Other dates")
	if err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("browser", 480*60)
	stamp := func(value string) int64 {
		parsed, err := time.ParseInLocation("2006-01-02 15:04", value, zone)
		if err != nil {
			t.Fatal(err)
		}
		return parsed.Unix()
	}
	seed := func(id, date, name, kind string, albumID int64, published, hidden bool) {
		f := telegramTestFile(id, 0)
		f.AlbumID = sql.NullInt64{Int64: albumID, Valid: true}
		if err := d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE files SET created_at=?,name=?,kind=?,published=?,hidden=? WHERE id=?`, stamp(date), name, kind, published, hidden, id); err != nil {
			t.Fatal(err)
		}
	}
	for i, date := range []string{
		"2023-12-31 23:59", "2024-01-01 00:00", "2024-01-07 23:59", "2024-01-08 00:00",
		"2024-02-28 23:59", "2024-02-29 00:00", "2024-02-29 23:59", "2024-03-01 00:00",
		"2024-03-03 23:59", "2024-03-04 00:00",
	} {
		seed(fmt.Sprintf("calendar-%d", i), date, "match-scope", "image", album, true, false)
	}
	seed("wrong-album", "2024-03-04 12:00", "match-scope", "image", otherAlbum, true, false)
	seed("wrong-state", "2024-03-04 12:00", "match-scope", "image", album, false, false)
	seed("wrong-kind", "2024-03-04 12:00", "match-scope", "video", album, true, false)
	seed("wrong-query", "2024-03-04 12:00", "different", "image", album, true, false)
	seed("hidden-date", "2024-03-04 12:00", "match-scope", "image", album, true, true)
	filter := FileFilter{Query: "match-scope", Kind: "image", State: "published", AlbumID: album}
	for _, test := range []struct {
		group  string
		keys   []string
		counts []int
	}{
		{"day", []string{"2024-03-04", "2024-03-03", "2024-03-01", "2024-02-29", "2024-02-28", "2024-01-08", "2024-01-07", "2024-01-01", "2023-12-31"}, []int{1, 1, 1, 2, 1, 1, 1, 1, 1}},
		{"week", []string{"2024-03-04", "2024-02-26", "2024-01-08", "2024-01-01", "2023-12-25"}, []int{1, 5, 1, 2, 1}},
		{"month", []string{"2024-03", "2024-02", "2024-01", "2023-12"}, []int{3, 3, 3, 1}},
		{"year", []string{"2024", "2023"}, []int{9, 1}},
	} {
		t.Run(test.group, func(t *testing.T) {
			// Loaded page, sort and active date seeking must not shrink the index.
			indexFilter := filter
			indexFilter.Before, indexFilter.After = stamp("2025-01-01 00:00"), stamp("2024-12-01 00:00")
			indexFilter.Page, indexFilter.PerPage, indexFilter.Sort = 999, 1, "oldest"
			groups, err := d.FileTimelineGroups(indexFilter, 480, test.group)
			if err != nil || len(groups) != len(test.keys) {
				t.Fatalf("wrong complete scope: %+v %v", groups, err)
			}
			for i, group := range groups {
				if group.Key != test.keys[i] || group.Count != test.counts[i] {
					t.Errorf("bucket %d: %+v, expected %s count %d", i, group, test.keys[i], test.counts[i])
				}
				start := time.Unix(group.Start, 0).In(zone)
				if start.Hour() != 0 || start.Minute() != 0 || start.Second() != 0 {
					t.Errorf("bucket does not start at local midnight: %+v", group)
				}
				end := start.AddDate(0, 0, 1)
				if test.group == "week" {
					if start.Weekday() != time.Monday {
						t.Errorf("week starts on %s", start.Weekday())
					}
					end = start.AddDate(0, 0, 7)
				} else if test.group == "month" {
					if start.Day() != 1 {
						t.Errorf("month starts on day %d", start.Day())
					}
					end = start.AddDate(0, 1, 0)
				}
				if test.group == "year" {
					end = start.AddDate(1, 0, 0)
					if start.Month() != time.January || start.Day() != 1 {
						t.Errorf("year start: %s", start)
					}
				}
				if group.End != end.Unix() {
					t.Errorf("wrong exclusive end: %+v, expected %d", group, end.Unix())
				}
				for _, sort := range []string{"newest", "oldest"} {
					rangeFilter := filter
					rangeFilter.After, rangeFilter.Before, rangeFilter.Sort = group.Start, group.End, sort
					files, total, err := d.ListFiles(rangeFilter)
					if err != nil || total != group.Count || len(files) != group.Count {
						t.Errorf("%s section fetch differs: total=%d files=%d bucket=%+v err=%v", sort, total, len(files), group, err)
					}
					for _, file := range files {
						if file.CreatedAt < group.Start || file.CreatedAt >= group.End {
							t.Errorf("file outside half-open section: %+v", file)
						}
					}
				}
			}
		})
	}
	if _, err := d.FileTimelineGroups(filter, 480, "decade"); err == nil {
		t.Fatal("invalid group accepted")
	}
}

func TestTimelineGroupsUseRequestedFixedOffset(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("offset-boundary", 0)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		date, group, east, west string
	}{
		{"2024-02-29T23:30:00Z", "day", "2024-03-01", "2024-02-29"},
		{"2023-12-31T20:00:00Z", "week", "2024-01-01", "2023-12-25"},
		{"2024-02-29T23:30:00Z", "month", "2024-03", "2024-02"},
		{"2023-12-31T20:00:00Z", "year", "2024", "2023"},
	} {
		stamp, err := time.Parse(time.RFC3339, test.date)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE files SET created_at=? WHERE id=?`, stamp.Unix(), f.ID); err != nil {
			t.Fatal(err)
		}
		for _, offset := range []struct {
			minutes int
			key     string
		}{{480, test.east}, {-300, test.west}} {
			groups, err := d.FileTimelineGroups(FileFilter{}, offset.minutes, test.group)
			if err != nil || len(groups) != 1 || groups[0].Key != offset.key || groups[0].Count != 1 {
				t.Fatalf("%s offset %d: %+v %v", test.group, offset.minutes, groups, err)
			}
			if stamp.Unix() < groups[0].Start || stamp.Unix() >= groups[0].End {
				t.Errorf("%s offset %d bounds do not include timestamp: %+v", test.group, offset.minutes, groups[0])
			}
		}
	}
}
