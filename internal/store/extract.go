package store

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// 归档解压。pimg 浏览的文件系统源目录可写时，把压缩包在「源目录旁」解压：
// 若压缩包内部根只有单一文件夹则直接落为同名目录；否则先建一个与压缩包同名
// 的文件夹再解入。解压后台执行、不阻塞，一次只跑一个。进度保存在内存。

const extractSlots = 1

type extractJob struct {
	Done    bool   `json:"done"`
	Ok      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Target  string `json:"target"`
	Percent int    `json:"percent"`
	done    chan struct{}
}

type extractCenter struct {
	mu    sync.Mutex
	slots chan struct{}
	jobs  map[string]*extractJob
	queue []string
}

func newExtractCenter() *extractCenter {
	return &extractCenter{slots: make(chan struct{}, extractSlots), jobs: map[string]*extractJob{}}
}

func (e *extractCenter) getJob(key string) *extractJob {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.jobs[key]
}

func (e *extractCenter) status(key string) *extractJob {
	e.mu.Lock()
	defer e.mu.Unlock()
	if j := e.jobs[key]; j != nil {
		c := *j
		return &c
	}
	return nil
}

// ArchiveFile reports whether an extension denotes a supported archive file.
func ArchiveFile(ext string) bool {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "zip", "rar", "7z", "iso":
		return true
	}
	return false
}

// ExtractStatus returns the current extraction job for an archive path.
func (s *Service) ExtractStatus(abs string) *extractJob {
	key := filepath.Clean(abs)
	if j := s.extract.getJob(key); j != nil {
		c := *j
		return &c
	}
	return nil
}

// StartExtract queues an archive for extraction next to its own directory.
func (s *Service) StartExtract(abs string) (*extractJob, error) {
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() {
		return nil, errors.New("文件不存在或不是普通文件")
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(abs), "."))
	if !ArchiveFile(ext) {
		return nil, errors.New("不支持的压缩格式")
	}
	key := filepath.Clean(abs)
	e := s.extract
	e.mu.Lock()
	existing := e.jobs[key]
	if existing != nil && !existing.Done {
		c := *existing
		e.mu.Unlock()
		return &c, nil
	}
	target := filepath.Join(filepath.Dir(abs), strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs)))
	job := &extractJob{Target: target, done: make(chan struct{})}
	e.jobs[key] = job
	e.mu.Unlock()
	go func() {
		e.slots <- struct{}{}
		defer func() { <-e.slots }()
		s.runOneExtract(key, job)
	}()
	return job, nil
}

func (s *Service) runOneExtract(key string, job *extractJob) {
	defer close(job.done)
	abs := key
	target := job.Target
	queue := filepath.Join(filepath.Dir(abs), ".pimg-extract-"+filepath.Base(abs))
	_ = os.RemoveAll(queue)
	_ = os.MkdirAll(queue, 0700)
	err := s.runExtractCommand(abs, queue, job)
	if err != nil {
		_ = os.RemoveAll(queue)
		e := s.extract
		e.mu.Lock()
		job.Done, job.Ok, job.Error = true, false, err.Error()
		e.mu.Unlock()
		return
	}
	// 智能落位
	finalized := target
	entries, _ := os.ReadDir(queue)
	if len(entries) == 1 && entries[0].IsDir() {
		inner := filepath.Join(queue, entries[0].Name())
		_ = os.RemoveAll(target)
		if merr := os.Rename(inner, target); merr == nil {
			finalized = target
			_ = os.RemoveAll(queue) // 父临时目录（已腾空）清理
		} else {
			_ = os.RemoveAll(queue)
			e := s.extract
			e.mu.Lock()
			job.Done, job.Ok, job.Error = false, false, merr.Error()
			e.mu.Unlock()
			return
		}
	} else {
		_ = os.RemoveAll(target)
		if merr := os.Rename(queue, target); merr != nil {
			_ = os.RemoveAll(queue)
			e := s.extract
			e.mu.Lock()
			job.Done, job.Ok, job.Error = false, false, merr.Error()
			e.mu.Unlock()
			return
		}
	}
	e := s.extract
	e.mu.Lock()
	job.Done, job.Ok, job.Target, job.Error, job.Percent = true, true, finalized, "", 100
	e.mu.Unlock()
}

func (s *Service) runExtractCommand(abs, out string, job *extractJob) error {
	args := []string{"x", "-y", "-bd", "-bsp1", "-o" + out, abs}
	cmd := exec.Command("7z", args...)
	cmd.Stderr = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	// 7z -bsp1 prints lines like " 23%  12 of 100..." ; parse the trailing %.
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if pct := parse7zPercent(line); pct >= 0 {
			e := s.extract
			e.mu.Lock()
			job.Percent = pct
			e.mu.Unlock()
		}
	}
	scErr := sc.Err()
	var exitErr string
	if werr := cmd.Wait(); werr != nil {
		var exit *exec.ExitError
		if errors.As(werr, &exit) {
			exitErr = "（code " + itoa(exit.ExitCode()) + "）"
		} else {
			exitErr = werr.Error()
		}
	}
	if scErr != nil {
		return errors.New("解压进度读取失败: " + scErr.Error())
	}
	if exitErr != "" {
		return errors.New("解压失败" + exitErr)
	}
	return nil
}

// parse7zPercent extracts a bare percentage from a 7z progress line, e.g.
// " 43%  110 of 300" → 43. Lines without a trailing % yield -1.
func parse7zPercent(line string) int {
	if i := strings.LastIndex(line, "%"); i >= 0 {
		var n int
		if _, err := fmt.Sscanf(line[:i], "%d", &n); err == nil {
			if n >= 0 && n <= 100 {
				return n
			}
		}
	}
	return -1
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}