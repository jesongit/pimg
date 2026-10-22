package dbx

// Keep intake timestamps for operations/logs. Gallery time uses the original
// content timestamp already captured before encryption or video segmentation.
// A single indexed expression keeps old rows, new intake, conversions, date
// seeking and calendar counts consistent without rewriting archive metadata.
const capturedMediaTimeSQL = `CASE WHEN json_valid(naming_json) THEN CASE
 WHEN json_extract(naming_json,'$.time_source') IN ('exif','video_metadata','file_created')
 AND json_type(naming_json,'$.original_time')='integer'
 AND json_extract(naming_json,'$.original_time') BETWEEN -2208988800 AND 253402300799
 THEN json_extract(naming_json,'$.original_time') ELSE created_at END ELSE created_at END`

// Keep the historical migration unchanged. Manual dates have a separate,
// automatic index migration; existing media rows are never rewritten.
const mediaTimeSchema = `CREATE INDEX files_media_time ON files((` + capturedMediaTimeSQL + `) DESC,id DESC);
CREATE INDEX files_owner_media_time ON files(owner_id,(` + capturedMediaTimeSQL + `) DESC,id DESC);`

const mediaTimeSQL = `CASE WHEN json_valid(naming_json) THEN CASE
 WHEN json_type(naming_json,'$.manual_time')='integer'
 AND json_extract(naming_json,'$.manual_time') BETWEEN -2208988800 AND 253402300799
 THEN json_extract(naming_json,'$.manual_time') ELSE (` + capturedMediaTimeSQL + `) END ELSE created_at END`

const manualMediaTimeSchema = `DROP INDEX files_media_time;
DROP INDEX files_owner_media_time;
CREATE INDEX files_media_time ON files((` + mediaTimeSQL + `) DESC,id DESC);
CREATE INDEX files_owner_media_time ON files(owner_id,(` + mediaTimeSQL + `) DESC,id DESC);`

func ValidMediaTime(value int64) bool { return value >= -2208988800 && value <= 253402300799 }

func (f *File) MediaTimeSource() string {
	if f.Naming.ManualTime != nil && ValidMediaTime(*f.Naming.ManualTime) {
		return "manual"
	}
	switch f.Naming.TimeSource {
	case "exif", "video_metadata", "file_created":
		if f.Naming.OriginalTime >= -2208988800 && f.Naming.OriginalTime <= 253402300799 {
			return f.Naming.TimeSource
		}
	}
	return "ingested"
}
