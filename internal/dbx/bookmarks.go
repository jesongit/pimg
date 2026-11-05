package dbx

import (
	"database/sql"
	"errors"
	"strings"
)

// A bookmark bar is a small tree per administrator account. Folders only group
// saved source directories; deleting a folder or an item never touches the
// referenced filesystem content.
const bookmarkSchema = `
CREATE TABLE file_bookmarks(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_id TEXT NOT NULL DEFAULT 'admin:1',
  parent_id INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL CHECK(kind IN ('folder','bookmark')),
  name TEXT NOT NULL,
  path TEXT NOT NULL DEFAULT '',
  position INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_file_bookmarks_owner_parent ON file_bookmarks(owner_id,parent_id,position,id);
`

// FileBookmarkNode is one entry in the bookmark tree. Folders carry children;
// bookmarks carry a source path.
type FileBookmarkNode struct {
	ID        int64               `json:"id"`
	Type      string              `json:"type"`
	Name      string              `json:"name"`
	Path      string              `json:"path,omitempty"`
	Directory bool                `json:"directory"`
	Children  []*FileBookmarkNode `json:"children,omitempty"`
}

var (
	ErrBookmarkNotFound  = errors.New("收藏不存在")
	ErrBookmarkFolder    = errors.New("收藏文件夹不存在")
	ErrBookmarkName      = errors.New("收藏名称无效")
	ErrBookmarkPathEmpty = errors.New("收藏目录无效")
	ErrBookmarkCycle     = errors.New("不能把收藏文件夹移动到自身或子文件夹")
)

func validBookmarkName(name string) bool {
	if name == "" || len(name) > 200 {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (db *DB) nextBookmarkPosition(owner string, parent int64) (int64, error) {
	var position int64
	err := db.QueryRow(`SELECT COALESCE(MAX(position)+1,0) FROM file_bookmarks WHERE owner_id=? AND parent_id=?`, owner, parent).Scan(&position)
	return position, err
}

// parentFolderExists accepts 0 (the bookmark bar root) or an existing folder
// owned by the same account.
func (db *DB) parentFolderExists(owner string, parent int64) (bool, error) {
	if parent == 0 {
		return true, nil
	}
	var exists bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM file_bookmarks WHERE id=? AND owner_id=? AND kind='folder')`, parent, owner).Scan(&exists)
	return exists, err
}

func (db *DB) ListFileBookmarks(owner string) ([]*FileBookmarkNode, error) {
	rows, err := db.ReadQuery(`SELECT id,parent_id,kind,name,path FROM file_bookmarks WHERE owner_id=? ORDER BY parent_id,position,id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byParent := map[int64][]*FileBookmarkNode{}
	all := []*FileBookmarkNode{}
	for rows.Next() {
		var id, parent int64
		var kind, name, path string
		if err = rows.Scan(&id, &parent, &kind, &name, &path); err != nil {
			return nil, err
		}
		node := &FileBookmarkNode{ID: id, Type: kind, Name: name, Path: path}
		byParent[parent] = append(byParent[parent], node)
		all = append(all, node)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// Folders are assigned their children after all nodes exist so nested
	// folders keep the same stable order as the flat read.
	for _, node := range all {
		if node.Type == "folder" {
			if list, ok := byParent[node.ID]; ok {
				node.Children = list
			}
		}
	}
	root := byParent[0]
	if root == nil {
		root = []*FileBookmarkNode{}
	}
	return root, nil
}

func (db *DB) CreateFileBookmarkFolder(owner string, parent int64, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if !validBookmarkName(name) {
		return 0, ErrBookmarkName
	}
	ok, err := db.parentFolderExists(owner, parent)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrBookmarkFolder
	}
	position, err := db.nextBookmarkPosition(owner, parent)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO file_bookmarks(owner_id,parent_id,kind,name,position,created_at) VALUES(?,?,'folder',?,?,?)`, owner, parent, name, position, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) RenameFileBookmarkFolder(owner string, id int64, name string) error {
	name = strings.TrimSpace(name)
	if !validBookmarkName(name) {
		return ErrBookmarkName
	}
	res, err := db.Exec(`UPDATE file_bookmarks SET name=? WHERE id=? AND owner_id=? AND kind='folder'`, name, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBookmarkFolder
	}
	return nil
}

// CreateFileBookmark saves a source directory into the bar or one of its
// folders. Repeating the same path in the same folder returns the existing id
// so a double click cannot create duplicates.
func (db *DB) CreateFileBookmark(owner string, folder int64, path, name string) (int64, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, ErrBookmarkPathEmpty
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = path
	}
	name = truncateBookmarkName(name)
	ok, err := db.parentFolderExists(owner, folder)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrBookmarkFolder
	}
	var existing int64
	err = db.QueryRow(`SELECT id FROM file_bookmarks WHERE owner_id=? AND parent_id=? AND kind='bookmark' AND path=? LIMIT 1`, owner, folder, path).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	position, err := db.nextBookmarkPosition(owner, folder)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO file_bookmarks(owner_id,parent_id,kind,name,path,position,created_at) VALUES(?,?,'bookmark',?,?,?,?)`, owner, folder, name, path, position, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func truncateBookmarkName(name string) string {
	if len(name) <= 200 {
		return name
	}
	// Keep the byte limit but avoid cutting a multi-byte rune in half.
	cut := 200
	for cut > 0 && (name[cut]&0xC0) == 0x80 {
		cut--
	}
	return name[:cut]
}

func (db *DB) DeleteFileBookmark(owner string, id int64) error {
	res, err := db.Exec(`DELETE FROM file_bookmarks WHERE id=? AND owner_id=? AND kind='bookmark'`, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBookmarkNotFound
	}
	return nil
}

// DeleteFileBookmarkFolder removes the folder and every nested folder/bookmark
// it contains. The referenced directories on disk are untouched.
func (db *DB) DeleteFileBookmarkFolder(owner string, id int64) error {
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM file_bookmarks WHERE id=? AND owner_id=? AND kind='folder')`, id, owner).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrBookmarkFolder
	}
	_, err := db.Exec(`WITH RECURSIVE sub(id) AS (
  SELECT id FROM file_bookmarks WHERE id=? AND owner_id=? AND kind='folder'
  UNION ALL
  SELECT b.id FROM file_bookmarks b JOIN sub ON b.parent_id=sub.id WHERE b.owner_id=?
)
DELETE FROM file_bookmarks WHERE owner_id=? AND id IN (SELECT id FROM sub)`, id, owner, owner, owner)
	return err
}

// MoveFileBookmark reparents a bookmark/folder and inserts it at position among
// its new siblings (0-based, excluding itself). A folder cannot be moved into
// itself or one of its own descendants.
func (db *DB) MoveFileBookmark(owner string, id, parent int64, position int) error {
	var kind string
	var oldParent int64
	err := db.QueryRow(`SELECT kind,parent_id FROM file_bookmarks WHERE id=? AND owner_id=?`, id, owner).Scan(&kind, &oldParent)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBookmarkNotFound
	}
	if err != nil {
		return err
	}
	ok, err := db.parentFolderExists(owner, parent)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBookmarkFolder
	}
	if kind == "folder" {
		var cycle bool
		if err = db.QueryRow(`WITH RECURSIVE sub(id) AS (
  SELECT id FROM file_bookmarks WHERE id=? AND owner_id=? AND kind='folder'
  UNION ALL
  SELECT b.id FROM file_bookmarks b JOIN sub ON b.parent_id=sub.id WHERE b.owner_id=?
)
SELECT EXISTS(SELECT 1 FROM sub WHERE id=?)`, id, owner, owner, parent).Scan(&cycle); err != nil {
			return err
		}
		if cycle {
			return ErrBookmarkCycle
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if oldParent != parent {
		if _, err = tx.Exec(`UPDATE file_bookmarks SET parent_id=? WHERE id=? AND owner_id=?`, parent, id, owner); err != nil {
			return err
		}
	}
	ids := []int64{}
	rows, err := tx.Query(`SELECT id FROM file_bookmarks WHERE owner_id=? AND parent_id=? AND id<>? ORDER BY position,id`, owner, parent, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var value int64
		if err = rows.Scan(&value); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if position < 0 {
		position = 0
	}
	if position > len(ids) {
		position = len(ids)
	}
	final := make([]int64, 0, len(ids)+1)
	final = append(final, ids[:position]...)
	final = append(final, id)
	final = append(final, ids[position:]...)
	for index, value := range final {
		if _, err = tx.Exec(`UPDATE file_bookmarks SET position=? WHERE id=? AND owner_id=?`, index, value, owner); err != nil {
			return err
		}
	}
	if oldParent != parent {
		oldIDs := []int64{}
		oldRows, e := tx.Query(`SELECT id FROM file_bookmarks WHERE owner_id=? AND parent_id=? ORDER BY position,id`, owner, oldParent)
		if e != nil {
			return e
		}
		for oldRows.Next() {
			var value int64
			if e = oldRows.Scan(&value); e != nil {
				oldRows.Close()
				return e
			}
			oldIDs = append(oldIDs, value)
		}
		e = oldRows.Err()
		oldRows.Close()
		if e != nil {
			return e
		}
		for index, value := range oldIDs {
			if _, e = tx.Exec(`UPDATE file_bookmarks SET position=? WHERE id=? AND owner_id=?`, index, value, owner); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}
