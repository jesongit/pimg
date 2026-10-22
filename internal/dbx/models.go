package dbx

import (
	"database/sql"
	"pimg/internal/filenames"
	"time"
)

// File 文件索引记录。
type File struct {
	MediaVersion     int
	MediaPolicies    []MediaReplica // Intake policy snapshot.
	OwnerID          string
	ID               string
	Name             string
	OrigName         string
	Ext              string
	Mime             string
	Size             int64
	SHA256           string
	Kind             string // image|video|other
	Width, Height    int
	Duration         float64
	KeyWrapped       []byte
	KeyNonce         []byte
	CryptoVer        int
	AlbumID          sql.NullInt64
	ArchiveState     string // pending|running|done|failed
	ArchiveError     sql.NullString
	ArchiveAttempts  int
	ArchivedAt       sql.NullInt64
	HasThumb         bool
	ThumbW, ThumbH   int
	ThumbMime        sql.NullString
	ThumbSize        int64
	HasBThumb        bool
	Published        bool
	PublishedAt      sql.NullInt64
	PublishedPath    sql.NullString
	PublishError     sql.NullString
	ShowHome         bool
	HomeVisibility   string // inherit|public|private; independent of legacy ShowHome.
	PurgeState       sql.NullString
	Storage          string // Legacy names: tg=encrypted archive, direct=plaintext archive. Publication is independent.
	NoAutoPublish    bool   // Intake policy persisted separately; legacy direct API still explicitly publishes.
	InputHash        string // Original bytes before video conversion/segmentation; transient intake metadata.
	InputSize        int64
	ColdBackend      string
	HotBackend       string
	Hidden           bool // Staging replacement or completed conversion receipt.
	CreatedAt        int64
	MediaAt          int64 // Gallery time; original capture/creation time, otherwise intake time.
	UpdatedAt        int64
	TelegramSource   string // Source message identity; survives deletion/conversion as a dedup tombstone.
	Naming           filenames.Metadata
	UploadDuplicate  bool  // Transient upload result only; never persisted to SQLite.
	OperationID      int64 // Intake-only operation boundary; committed with the archive job.
	OperationItemKey string
	ParentTaskID     int64 // Internal receive/scan parent, separate from browser input.
}

type Conversion struct {
	Kind            string
	TargetOwnerID   string
	TargetAlbumID   int64
	SourceID        string
	TargetID        string
	TargetEncrypted bool
	Phase           string
	TaskID          int64
	State           string
	LastError       string
	CleanupPending  bool
	CleanupError    string
}

// ExpiredAtTime 便捷时间访问。
func (f *File) CreatedAtTime() time.Time { return time.Unix(f.CreatedAt, 0) }

// Chunk 冷存储归档分片记录。
type Chunk struct {
	ID         int64
	FileID     string
	Kind       string // data|thumb|bthumb
	Idx        int
	MessageID  sql.NullInt64
	TgFileID   sql.NullString
	ObjectKey  string
	Size       int64
	SHA256     string
	UploadedAt sql.NullInt64
}

// TempObject 热存储临时区域对象。
type TempObject struct {
	ID        string
	FileID    string
	Kind      string // original|thumb
	ObjectKey string
	Backend   string
	Size      sql.NullInt64
	CreatedAt int64
	ExpiresAt int64
	DeletedAt sql.NullInt64
	ReadyAt   sql.NullInt64 // Successful upload or subsequent availability check.
}

// Live 是否仍在保留期内。
func (t *TempObject) Live(nowSec int64) bool { return t.DeletedAt.Int64 == 0 && t.ExpiresAt > nowSec }

// Task 异步任务。
type Task struct {
	ID          int64
	Type        string
	FileID      sql.NullString
	Payload     string
	State       string
	Attempts    int
	MaxAttempts int
	LastError   sql.NullString
	Result      sql.NullString
	RunAfter    int64
	LeaseUntil  sql.NullInt64
	CreatedAt   int64
	UpdatedAt   int64
	DoneAt      sql.NullInt64
}

// APIToken API 令牌。
type APIToken struct {
	Scope      string
	OwnerID    string
	TopicIDs   []int
	ID         string
	Name       string
	TokenHash  string
	Prefix     string
	CreatedAt  int64
	LastUsedAt sql.NullInt64
	RevokedAt  sql.NullInt64
}

// Album 相册。
type Album struct {
	OwnerID   string
	ID        int64
	Name      string
	CreatedAt int64
	Count     int `sql:"-"`
	IsHome    bool
	IsDefault bool
	Displayed bool
}

// TopicBinding keeps Telegram identity independent of mutable display names.
type TopicBinding struct {
	AlbumID       int64  `json:"album_id"`
	AccountID     int64  `json:"account_id"`
	ChatID        int64  `json:"chat_id"`
	TopicID       int    `json:"topic_id"`
	Name          string `json:"name"`
	CreateName    string `json:"-"`
	IsDefault     bool   `json:"is_default"`
	State         string `json:"state"`
	DeletePhase   string `json:"delete_phase,omitempty"`
	LastError     string `json:"error,omitempty"`
	ScanDate      int64  `json:"scan_date"`
	ScanMessageID int    `json:"scan_message_id"`
	ScanOffset    int    `json:"scan_offset"`
	ScanTaskID    int64  `json:"scan_task_id,omitempty"`
}

// TelegramMessage is a durable inbox item or a managed/retired archive locator.
// A file_id deliberately has no FK: deleting a file must not resurrect its source.
type TelegramMessage struct {
	AccountID int64
	ChatID    int64
	MessageID int
	TopicID   int
	Date      int64
	Reference string
	Name      string
	MIME      string
	Caption   string
	IsService bool
	State     string // pending|done|failed|ignored|managed|retired
	FileID    string
	LastError string
}

type TelegramMove struct {
	FileID     string
	AlbumID    int64
	Phase      string
	References string
	LastError  string
	CreatedAt  int64
	UpdatedAt  int64
}

type TelegramSourcePart struct {
	SourceKey string
	PartIndex int
	FileID    string
	PurgedAt  int64
}

// Session 管理员会话。
type Session struct {
	OwnerID   string
	TokenHash string
	CreatedAt int64
	ExpiresAt int64
	UA        sql.NullString
	IP        sql.NullString
}
