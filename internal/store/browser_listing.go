package store

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"pimg/internal/ids"
	"pimg/internal/mediax"
)

type BrowserOptions struct {
	Recursive bool
	Snapshot  string
}

type browserSnapshot struct {
	listing BrowserListing
	key     string
	expires time.Time
	bytes   int
}

type browserCache struct {
	mu        sync.Mutex
	snapshots []browserSnapshot
	scan      chan struct{}
}

const browserCacheBytes = 24 << 20
const browserMaxEntries = 50_000
const browserMaxVisited = 100_000

func (c *browserCache) scanSlot() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scan == nil {
		c.scan = make(chan struct{}, 1)
	}
	return c.scan
}

func (c *browserCache) get(token, key string) (BrowserListing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, saved := range c.snapshots {
		if saved.listing.Snapshot == token && saved.key == key && time.Now().Before(saved.expires) {
			return saved.listing, true
		}
	}
	return BrowserListing{}, false
}

func (c *browserCache) save(key string, listing BrowserListing, size int) BrowserListing {
	c.mu.Lock()
	defer c.mu.Unlock()
	listing.Snapshot = ids.New()
	retained, bytes := make([]browserSnapshot, 0, 2), size
	// At most two snapshots, with a shared byte budget. Refresh always scans
	// again; paging keeps a stable order without rescanning the whole tree.
	for i := len(c.snapshots) - 1; i >= 0; i-- {
		saved := c.snapshots[i]
		if len(retained) < 1 && time.Now().Before(saved.expires) && bytes+saved.bytes <= browserCacheBytes {
			retained = append(retained, saved)
			bytes += saved.bytes
		}
	}
	c.snapshots = append(retained, browserSnapshot{listing: listing, key: key, expires: time.Now().Add(10 * time.Minute), bytes: size})
	return listing
}

func browserCreationTime(ctx context.Context, root *os.Root, relative string, entry *BrowserEntry) {
	if ctx.Err() != nil {
		return
	}
	if _, err := importInfo(root, filepath.ToSlash(relative)); err != nil {
		return
	}
	file, err := root.Open(relative)
	if err != nil {
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || localVersion(info) != entry.Version {
		return
	}
	if created, ok := mediax.FileCreatedTime(file); ok {
		entry.CreatedAt, entry.createdNS = created.Unix(), created.UnixNano()
	}
}

func sortBrowserEntries(items []BrowserEntry, ordering string) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Directory != b.Directory {
			return a.Directory
		}
		if !a.Directory {
			switch ordering {
			case "size_desc", "size_asc":
				if a.Size != b.Size {
					return (a.Size > b.Size) == (ordering == "size_desc")
				}
			case "type", "type_desc":
				if a.Ext != b.Ext {
					return (a.Ext > b.Ext) == (ordering == "type_desc")
				}
			case "created_desc", "created_asc", "modified_desc", "modified_asc":
				x, y := a.modifiedNS, b.modifiedNS
				if strings.HasPrefix(ordering, "created_") {
					x, y = a.createdNS, b.createdNS
					if (x == 0) != (y == 0) {
						return x != 0 // Missing birth times stay last in either direction.
					}
				}
				if x != y {
					return (x > y) == strings.HasSuffix(ordering, "_desc")
				}
			}
		}
		nameOrder := cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		if nameOrder == 0 {
			nameOrder = cmp.Compare(a.Name, b.Name)
		}
		if nameOrder == 0 {
			nameOrder = cmp.Compare(a.RelativePath, b.RelativePath)
		}
		if ordering == "name_desc" {
			return nameOrder > 0
		}
		return nameOrder < 0
	})
}

func (s *Service) BrowseLocalFiles(ctx context.Context, p, query string, offset int, ordering string, options ...BrowserOptions) (BrowserListing, error) {
	switch ordering {
	case "", "name", "name_desc", "size_desc", "size_asc", "type", "type_desc", "created_desc", "created_asc", "modified_desc", "modified_asc":
	default:
		return BrowserListing{}, errors.New("目录排序参数无效")
	}
	if offset < 0 || offset > 10_000_000 || len(query) > 512 {
		return BrowserListing{}, errors.New("目录查询参数无效")
	}
	var opts BrowserOptions
	if len(options) > 0 {
		opts = options[0]
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if p == "" {
		p = s.InitialImportPath()
	}
	node, err := s.ImportPathInfo(p)
	if err != nil {
		return BrowserListing{}, err
	}
	if !node.Directory {
		node.Path = filepath.Dir(node.Path)
	}
	root, err := openImportRoot(node.Path)
	if err != nil {
		return BrowserListing{}, err
	}
	defer root.Close()
	query = strings.ToLower(strings.TrimSpace(query))
	if opts.Recursive {
		query = strings.ReplaceAll(query, `\`, "/")
	}
	key := node.Path + "\x00" + query + "\x00" + ordering
	var out BrowserListing
	if opts.Recursive && offset > 0 && opts.Snapshot != "" {
		var ok bool
		out, ok = s.browser.get(opts.Snapshot, key)
		if !ok {
			return BrowserListing{}, errors.New("文件列表已过期，请刷新后继续浏览")
		}
	} else {
		if opts.Recursive {
			slot := s.browser.scanSlot()
			select {
			case slot <- struct{}{}:
				defer func() { <-slot }()
			case <-ctx.Done():
				return BrowserListing{}, ctx.Err()
			}
		}
		out = BrowserListing{Path: node.Path, Parent: filepath.Dir(node.Path), Items: []BrowserEntry{}, Recursive: opts.Recursive}
		for current := node.Path; ; current = filepath.Dir(current) {
			name := filepath.Base(current)
			if current == filepath.Dir(current) {
				name = current
			}
			out.Breadcrumbs = append([]BrowserLocation{{Path: current, Name: name}}, out.Breadcrumbs...)
			if current == filepath.Dir(current) {
				break
			}
		}
		visited, bytes := 0, 0
		var walk func(*os.Root, string, int) error
		walk = func(current *os.Root, relative string, depth int) error {
			dir, err := current.Open(".")
			if err != nil {
				return err
			}
			defer dir.Close()
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				entries, readErr := dir.ReadDir(256)
				for _, item := range entries {
					if err := ctx.Err(); err != nil {
						return err
					}
					visited++
					if visited > browserMaxVisited || depth > 128 {
						return errors.New("目录内容过多或层级过深，请进入更小的子文件夹后展开")
					}
					if item.Type()&os.ModeSymlink != 0 {
						continue
					}
					info, err := item.Info()
					if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
						continue
					}
					rel := filepath.Join(relative, item.Name())
					if opts.Recursive && info.IsDir() {
						child, e := current.OpenRoot(item.Name())
						if e != nil {
							out.SkippedDirectories++
							continue
						}
						opened, e := child.Lstat(".")
						if e == nil && os.SameFile(info, opened) {
							e = walk(child, rel, depth+1)
						} else if e == nil {
							e = os.ErrNotExist
						}
						child.Close()
						if e != nil {
							if errors.Is(e, os.ErrPermission) || errors.Is(e, os.ErrNotExist) {
								out.SkippedDirectories++
								continue
							}
							return e
						}
						continue
					}
					match := item.Name()
					if opts.Recursive {
						match = filepath.ToSlash(rel)
					}
					if query != "" && !strings.Contains(strings.ToLower(match), query) {
						continue
					}
					entry := s.browserEntry(filepath.Join(node.Path, rel), info)
					if opts.Recursive && entry.Kind != "image" && entry.Kind != "video" {
						continue
					}
					entry.RelativePath = filepath.ToSlash(rel)
					if strings.HasPrefix(ordering, "created_") && !entry.Directory {
						browserCreationTime(ctx, root, rel, &entry)
					}
					bytes += 192 + len(entry.Path) + len(entry.RelativePath) + len(entry.Name) + len(entry.MIME) + len(entry.Version) + len(entry.Ext)
					if len(out.Items) >= browserMaxEntries || bytes > browserCacheBytes {
						return errors.New("文件列表过大，请进入更小的子文件夹后浏览")
					}
					out.Items = append(out.Items, entry)
				}
				if errors.Is(readErr, io.EOF) {
					return nil
				}
				if readErr != nil {
					return readErr
				}
			}
		}
		if err = walk(root, "", 0); err != nil {
			return BrowserListing{}, err
		}
		sortBrowserEntries(out.Items, ordering)
		out.Total = len(out.Items)
		if opts.Recursive {
			out = s.browser.save(key, out, bytes)
		}
	}
	start := min(offset, out.Total)
	end := min(start+200, out.Total)
	if end < out.Total {
		out.NextOffset = end
	}
	// Do not mutate snapshot entries while another request is paging them.
	out.Items = append([]BrowserEntry{}, out.Items[start:end]...)
	for i := range out.Items {
		if !strings.HasPrefix(ordering, "created_") || out.Items[i].Directory {
			browserCreationTime(ctx, root, filepath.FromSlash(out.Items[i].RelativePath), &out.Items[i])
		}
	}
	if err := ctx.Err(); err != nil {
		return BrowserListing{}, err
	}
	return out, nil
}
