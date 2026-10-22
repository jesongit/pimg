package server

// Opt-in, reproducible performance acceptance. It uses real HTTP, SQLite,
// encrypted archives and local storage; no existing application data is read.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/store"
	"pimg/internal/tasks"
)

func TestPreviewPerformance(t *testing.T) {
	output := os.Getenv("PIMG_PERF_OUTPUT")
	if output == "" {
		t.Skip("set PIMG_PERF_OUTPUT for isolated performance acceptance")
	}
	ctx := context.Background()
	dir := t.TempDir()
	h := httptest.NewUnstartedServer(nil)
	t.Cleanup(h.Close)
	cfg := &config.Config{Server: config.Server{PublicBaseURL: "http://" + h.Listener.Addr().String()}, Storage: config.Storage{Cold: "local", Hot: "local"}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, Upload: config.Upload{MaxSizeMB: 128, AllowedExts: []string{"png"}}, Workers: 1}
	db, err := dbx.Open(filepath.Join(dir, "performance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	svc, err := store.New(cfg, db, master, nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	engine := tasks.New(db, svc, 1, log)
	s, err := New(cfg, db, svc, engine, log)
	if err != nil {
		t.Fatal(err)
	}
	h.Config.Handler = s.Handler()
	h.Start()
	const session = "isolated-performance-session"
	hash := sha256.Sum256([]byte(session))
	if err := db.InsertSession(hex.EncodeToString(hash[:]), time.Now().Add(time.Hour).Unix(), "benchmark", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	db.EnsureAdmin("performance", "unused")
	request := func(method, address, body string) *http.Response {
		t.Helper()
		if strings.HasPrefix(address, "/") {
			address = h.URL + address
		}
		req, _ := http.NewRequest(method, address, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		r, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 200 {
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			t.Fatalf("%s: %d %s", address, r.StatusCode, b)
		}
		return r
	}
	jsonRequest := func(method, address, body string, target any) {
		t.Helper()
		r := request(method, address, body)
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(target); err != nil {
			t.Fatal(err)
		}
	}
	// Fixed seed and dimensions keep image sizes/content identical before/after.
	img := image.NewNRGBA(image.Rect(0, 0, 960, 720))
	random := rand.New(rand.NewSource(42))
	random.Read(img.Pix)
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var imageData bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&imageData, img); err != nil {
		t.Fatal(err)
	}
	seed := func(prefix string, n int) []string {
		t.Helper()
		ids := make([]string, 0, n)
		for i := 0; i < n; i++ {
			f, err := svc.ReceiveUpload(ctx, bytes.NewReader(imageData.Bytes()), fmt.Sprintf("%s-%02d.png", prefix, i), "png", sql.NullInt64{}, nil, 0, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.RunArchive(ctx, f.ID); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, f.ID)
		}
		return ids
	}
	idleIDs, busyIDs := seed("idle", 12), seed("busy", 12)
	seed("browser", 24)
	large := make([]byte, 32<<20)
	random.Read(large)
	largeHash := sha256.Sum256(large)
	largeIDs := []string{}
	var ciphertext []byte
	var decryptKey []byte
	for i := 0; i < 3; i++ {
		key := cryptox.GenerateFileKey()
		wrapped, nonce, _ := master.WrapFileKey(key)
		ct, err := cryptox.EncryptBytes(large, key)
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("large-%d", i)
		file := &dbx.File{ID: id, Name: id, OrigName: id + ".bin", Ext: "bin", Mime: "application/octet-stream", Size: int64(len(large)), SHA256: hex.EncodeToString(largeHash[:]), Kind: "other", KeyWrapped: wrapped, KeyNonce: nonce, CryptoVer: 1, ArchiveState: "done", Storage: "tg", ColdBackend: "local"}
		if err := db.InsertFile(file); err != nil {
			t.Fatal(err)
		}
		keyPath := id + "/data-0000.bin"
		filename := filepath.Join(cfg.Storage.LocalColdDir, filepath.FromSlash(keyPath))
		os.MkdirAll(filepath.Dir(filename), 0700)
		if err := os.WriteFile(filename, ct, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(ct)
		if err := db.UpsertChunk(&dbx.Chunk{FileID: id, Kind: "data", Idx: 0, ObjectKey: keyPath, Size: int64(len(ct)), SHA256: hex.EncodeToString(sum[:])}); err != nil {
			t.Fatal(err)
		}
		largeIDs = append(largeIDs, id)
		ciphertext, decryptKey = ct, key
	}
	db.Exec("UPDATE tasks SET state='done'") // Fixture archives were run synchronously.
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Stop)
	ms := func(start time.Time) float64 { return float64(time.Since(start).Microseconds()) / 1000 }
	measureBatch := func(ids []string) map[string]any {
		address := "/api/admin/files/previews?ids=" + strings.Join(ids, ",")
		start := time.Now()
		var result struct {
			Previews map[string]store.TempViewResult `json:"previews"`
		}
		jsonRequest("GET", address, "", &result)
		initial := ms(start)
		first, all := 0.0, 0.0
		poll := 0
		for time.Since(start) < 30*time.Second {
			ready := 0
			for _, v := range result.Previews {
				if v.Status == "ready" && v.URL != "" {
					ready++
				}
			}
			if ready > 0 && first == 0 {
				first = ms(start)
			}
			if ready == len(ids) {
				all = ms(start)
				break
			}
			time.Sleep(20 * time.Millisecond)
			jsonRequest("GET", address, "", &result)
			poll++
		}
		if all == 0 {
			t.Fatal("preview batch timed out")
		}
		fetchStart := time.Now()
		for _, id := range ids {
			r := request("GET", result.Previews[id].URL, "")
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
		return map[string]any{"link_response_ms": initial, "first_link_ms": first, "all_links_ms": all, "all_image_fetch_ms": ms(fetchStart), "polls": poll, "count": len(ids)}
	}
	report := map[string]any{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "cpu_count": runtime.NumCPU(), "scope": "real loopback HTTP, SQLite and local encrypted archives; no external Telegram/R2", "image_bytes": imageData.Len(), "original_bytes": len(large), "workers": 1, "date": time.Now().UTC().Format(time.RFC3339)}
	decryptTimes := []float64{}
	for i := 0; i < 7; i++ {
		start := time.Now()
		reader, err := cryptox.DecryptReader(bytes.NewReader(ciphertext), decryptKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Fatal(err)
		}
		decryptTimes = append(decryptTimes, ms(start))
	}
	report["decrypt_32m_ms"] = decryptTimes
	report["idle_cold_batch"] = measureBatch(idleIDs)
	warm := []float64{}
	for i := 0; i < 7; i++ {
		start := time.Now()
		var result any
		jsonRequest("GET", "/api/admin/files/previews?ids="+strings.Join(idleIDs, ","), "", &result)
		warm = append(warm, ms(start))
	}
	report["warm_batch_ms"] = warm
	// Real competing work: sixteen full 32 MiB archive integrity checks.
	for i := 0; i < 16; i++ {
		if _, err := db.EnqueueTask(dbx.TaskVerify, largeIDs[i%len(largeIDs)], "{}"); err != nil {
			t.Fatal(err)
		}
	}
	engine.Wake()
	report["busy_cold_batch"] = measureBatch(busyIDs)
	// Drain competing checks before comparing original access.
	for {
		var pending int
		db.QueryRow("SELECT COUNT(*) FROM tasks WHERE state IN ('pending','running')").Scan(&pending)
		if pending == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	measureOriginal := func(id string) map[string]any {
		start := time.Now()
		var view store.TempViewResult
		jsonRequest("POST", "/api/admin/files/"+id+"/temp", `{"kind":"original"}`, &view)
		polls := 0
		for view.Status != "ready" && time.Since(start) < 30*time.Second {
			time.Sleep(20 * time.Millisecond)
			jsonRequest("POST", "/api/admin/files/"+id+"/temp", `{"kind":"original"}`, &view)
			polls++
		}
		if view.Status != "ready" {
			t.Fatal("original timed out")
		}
		link := ms(start)
		r := request("GET", view.URL, "")
		buf := make([]byte, 64<<10)
		n, err := io.ReadFull(r.Body, buf)
		if err != nil {
			t.Fatal(err)
		}
		first := ms(start)
		hash := sha256.New()
		hash.Write(buf[:n])
		_, err = io.Copy(hash, r.Body)
		r.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(hash.Sum(nil), largeHash[:]) {
			t.Fatal("original bytes differ")
		}
		return map[string]any{"link_ms": link, "first_64k_ms": first, "total_ms": ms(start), "polls": polls, "sha256_verified": true}
	}
	originals := []map[string]any{}
	for _, id := range largeIDs {
		originals = append(originals, measureOriginal(id))
	}
	report["cold_originals"] = originals
	warmOriginals := []map[string]any{}
	for i := 0; i < 3; i++ {
		warmOriginals = append(warmOriginals, measureOriginal(largeIDs[0]))
	}
	report["warm_originals"] = warmOriginals
	if script := os.Getenv("PIMG_PERF_BROWSER"); script != "" {
		command := exec.Command(os.Getenv("PIMG_PERF_NODE"), script)
		command.Env = append(os.Environ(), "PIMG_PERF_URL="+h.URL, "PIMG_PERF_SESSION="+session, "PIMG_PERF_STAGE="+os.Getenv("PIMG_PERF_STAGE"))
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("browser: %v\n%s", err, out)
		}
		t.Log(string(out))
		report["browser_report"] = os.Getenv("PIMG_PERF_STAGE") + "-browser.json"
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(output, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log(string(data))
}
