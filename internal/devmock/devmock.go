// Package devmock 提供本地开发验证用的 Telegram/R2 假服务，
// 使完整闭环（上传→加密归档→临时查看→发布→游客访问）可在无真实凭据下跑通。
// 仅在 dev_mock 模式启用；数据保存在内存中，重启即失。
package devmock

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// TGServer 假 Telegram Bot API。
type TGServer struct {
	mu     sync.Mutex
	files  map[string][]byte // file_id -> bytes
	srv    *http.Server
	Base   string
}

// StartTG 启动假 TG 服务。
func StartTG(log *slog.Logger) (*TGServer, error) {
	t := &TGServer{files: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/getMe") || strings.HasSuffix(p, "/getChat"):
			writeTG(w, map[string]any{"id": 1, "type": "channel", "title": "mock"})
		case strings.HasSuffix(p, "/sendDocument"):
			if err := r.ParseMultipartForm(64 << 20); err != nil {
				writeTGErr(w, err)
				return
			}
			f, hdr, err := r.FormFile("document")
			if err != nil {
				writeTGErr(w, err)
				return
			}
			data, _ := io.ReadAll(f)
			f.Close()
			t.mu.Lock()
			id := "mock-" + strconv.Itoa(len(t.files)) + "-" + hex.EncodeToString([]byte(hdr.Filename)[:min(4, len(hdr.Filename))])
			t.files[id] = data
			n := len(t.files)
			t.mu.Unlock()
			writeTG(w, map[string]any{
				"message_id": 1000 + n,
				"document":   map[string]any{"file_id": id, "file_size": len(data)},
			})
		case strings.HasSuffix(p, "/getFile"):
			if err := r.ParseForm(); err != nil {
				writeTGErr(w, err)
				return
			}
			body := struct{ FileID string }{FileID: r.FormValue("file_id")}
			if _, ok := t.files[body.FileID]; !ok {
				writeTGErr(w, fmt.Errorf("file not found"))
				return
			}
			writeTG(w, map[string]any{"file_id": body.FileID, "file_path": "f/" + body.FileID})
		case strings.Contains(p, "/file/"):
			id := p[strings.LastIndex(p, "/file/")+len("/file/"):]
			// 跳过 bot<token>/ 前缀，再跳过 f/ 前缀
			if idx := strings.Index(id, "/"); idx >= 0 {
				id = id[idx+1:]
			}
			id = strings.TrimPrefix(id, "f/")
			t.mu.Lock()
			data, ok := t.files[id]
			t.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(data)
		case strings.HasSuffix(p, "/deleteMessage"):
			writeTG(w, map[string]bool{})
		default:
			log.Info("mock tg 请求", "path", p)
			writeTG(w, map[string]bool{})
		}
	})
	return t, t.listen(mux)
}

// Close 关闭假服务。
func (t *TGServer) Close() error {
	if t.srv != nil {
		return t.srv.Close()
	}
	return nil
}

func (t *TGServer) listen(mux *http.ServeMux) error {
	l, err := listenLocal()
	if err != nil {
		return err
	}
	t.srv = &http.Server{Handler: mux}
	t.Base = "http://" + l.Addr().String()
	go t.srv.Serve(l)
	return nil
}

// S3Server 假 S3/R2 服务。
type S3Server struct {
	mu    sync.Mutex
	objs  map[string][]byte // bucket/key -> data
	srv   *http.Server
	Base  string
}

// StartS3 启动假 S3 服务。
func StartS3() (*S3Server, error) {
	s := &S3Server{objs: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		switch r.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.objs[key] = data
			s.mu.Unlock()
			w.WriteHeader(200)
		case http.MethodHead:
			s.mu.Lock()
			data, ok := s.objs[key]
			s.mu.Unlock()
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(200)
		case http.MethodGet:
			s.mu.Lock()
			data, ok := s.objs[key]
			s.mu.Unlock()
			if !ok {
				w.WriteHeader(404)
				io.WriteString(w, "NoSuchKey")
				return
			}
			// 回显预签名 URL 携带的响应头参数（真实 S3/R2 行为）
			if cc := r.URL.Query().Get("response-cache-control"); cc != "" {
				w.Header().Set("Cache-Control", cc)
			}
			// 支持 Range（视频拖动需要）
			if rg := r.Header.Get("Range"); rg != "" {
				var start, end int
				if strings.HasPrefix(rg, "bytes=") {
					part := strings.TrimPrefix(rg, "bytes=")
					fmt.Sscanf(part, "%d-%d", &start, &end)
					if end == 0 || end >= len(data) {
						end = len(data) - 1
					}
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
					w.WriteHeader(206)
					w.Write(data[start : end+1])
					return
				}
			}
			w.Write(data)
		case http.MethodDelete:
			s.mu.Lock()
			delete(s.objs, key)
			s.mu.Unlock()
			w.WriteHeader(204)
		default:
			w.WriteHeader(405)
		}
	})
	return s, s.listen(mux)
}

func (s *S3Server) listen(mux *http.ServeMux) error {
	l, err := listenLocal()
	if err != nil {
		return err
	}
	s.srv = &http.Server{Handler: mux}
	s.Base = "http://" + l.Addr().String()
	go s.srv.Serve(l)
	return nil
}

// Close 关闭假服务。
func (s *S3Server) Close() error {
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

func writeTG(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func writeTGErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": err.Error()})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func listenLocal() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

