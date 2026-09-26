package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed static
var staticFS embed.FS

// indexHTML is read once at startup so the hot path stays a plain write.
var indexHTML []byte

func init() {
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		panic("static/index.html missing from build: " + err.Error())
	}
	indexHTML = data
}

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomID(n int) string {
	b := make([]byte, n)
	for i := range b {
		idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		b[i] = charset[idx.Int64()]
	}
	return string(b)
}

// Config is persisted to config.json.
type Config struct {
	Port             int    `json:"port"`
	MaxFileSizeMB    int    `json:"max_file_size_mb"`
	ShareExpiryHours int    `json:"share_expiry_hours"`
	StorageDir       string `json:"storage_dir"`
}

var defaultConfig = Config{
	Port:             8080,
	MaxFileSizeMB:    500,
	ShareExpiryHours: 24,
	StorageDir:       "uploads",
}

type LinkInfo struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

type FileInfo struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
	StoredName string    `json:"stored_name"`
}

type Share struct {
	ID        string     `json:"id"`
	Files     []FileInfo `json:"files,omitempty"`
	Links     []LinkInfo `json:"links,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
}

// Store holds share metadata and persists to a JSON file.
type Store struct {
	mu     sync.RWMutex
	shares map[string]*Share
	path   string
}

func newStore(path string) (*Store, error) {
	s := &Store{shares: make(map[string]*Share), path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	return s, json.Unmarshal(data, &s.shares)
}

func (s *Store) save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.shares, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

func (s *Store) add(share *Share) error {
	s.mu.Lock()
	s.shares[share.ID] = share
	s.mu.Unlock()
	return s.save()
}

func (s *Store) delete(id string) (*Share, bool) {
	s.mu.Lock()
	share, ok := s.shares[id]
	if ok {
		delete(s.shares, id)
	}
	s.mu.Unlock()
	if ok {
		s.save()
	}
	return share, ok
}

func (s *Store) get(id string) (*Share, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	share, ok := s.shares[id]
	return share, ok
}

func (s *Store) list() []*Share {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Share, 0, len(s.shares))
	for _, sh := range s.shares {
		out = append(out, sh)
	}
	return out
}

func (s *Store) cleanup(storageDir string) int {
	s.mu.Lock()
	var expired []string
	for id, sh := range s.shares {
		if time.Now().After(sh.ExpiresAt) {
			expired = append(expired, id)
		}
	}
	for _, id := range expired {
		for _, f := range s.shares[id].Files {
			os.Remove(filepath.Join(storageDir, f.StoredName))
		}
		delete(s.shares, id)
	}
	s.mu.Unlock()
	if len(expired) > 0 {
		s.save()
	}
	return len(expired)
}

// rateLimiter is a simple per-key token bucket.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rlBucket
}

type rlBucket struct {
	tokens   float64
	lastTime time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*rlBucket)}
}

func (rl *rateLimiter) allow(key string, rate, burst float64) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &rlBucket{tokens: burst, lastTime: time.Now()}
		rl.buckets[key] = b
	}
	now := time.Now()
	b.tokens += now.Sub(b.lastTime).Seconds() * rate
	b.lastTime = now
	if b.tokens > burst {
		b.tokens = burst
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	var sb strings.Builder
	for _, r := range name {
		if r == '.' || r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	s := sb.String()
	if s == "" || s == "." {
		return "file"
	}
	return s
}

func localIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "localhost"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "localhost"
}

func ipOf(r *http.Request) string {
	ip := r.RemoteAddr
	if i := strings.LastIndex(ip, ":"); i >= 0 {
		ip = ip[:i]
	}
	return ip
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// FileOut and ShareOut mirror FileInfo/Share for API responses, adding a
// ready-to-use download URL computed from the request instead of stored.
type FileOut struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
	URL        string    `json:"url"`
}

type ShareOut struct {
	ID        string     `json:"id"`
	Files     []FileOut  `json:"files,omitempty"`
	Links     []LinkInfo `json:"links,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
}

func toShareOut(sh *Share, base string) ShareOut {
	out := ShareOut{ID: sh.ID, Links: sh.Links, CreatedAt: sh.CreatedAt, ExpiresAt: sh.ExpiresAt}
	for _, f := range sh.Files {
		out.Files = append(out.Files, FileOut{
			Name:       f.Name,
			Size:       f.Size,
			UploadedAt: f.UploadedAt,
			URL:        base + "/d/" + sh.ID + "/" + url.PathEscape(f.Name),
		})
	}
	return out
}

func dirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	return total
}

type server struct {
	cfg     *Config
	cfgPath string
	store   *Store
	rl      *rateLimiter
	mux     *http.ServeMux
}

func newServer(cfg *Config, cfgPath string, store *Store) *server {
	s := &server{
		cfg:     cfg,
		cfgPath: cfgPath,
		store:   store,
		rl:      newRateLimiter(),
		mux:     http.NewServeMux(),
	}
	assets, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("static assets unavailable: " + err.Error())
	}
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(assets))))

	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/upload", s.handleUpload)
	s.mux.HandleFunc("/api/upload/file", s.handleUploadSingle)
	s.mux.HandleFunc("/api/shares", s.handleShares)
	s.mux.HandleFunc("/api/shares/", s.handleShareOp)
	s.mux.HandleFunc("/api/settings", s.handleSettings)
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/links", s.handleLinks)
	s.mux.HandleFunc("/d/", s.handleDownload)
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s %s", r.Method, r.URL.Path, ipOf(r))
	s.mux.ServeHTTP(w, r)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := ipOf(r)
	// 10 uploads/min burst per IP
	if !s.rl.allow(ip+"_up", 10.0/60.0, 10) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	maxBytes := int64(s.cfg.MaxFileSizeMB) * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes*20+1024) // allow multiple files

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "request too large or malformed", http.StatusBadRequest)
		return
	}

	fhs := r.MultipartForm.File["files"]
	if len(fhs) == 0 {
		http.Error(w, "no files provided", http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(s.cfg.StorageDir, 0755); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	var uploaded []FileInfo
	for _, fh := range fhs {
		if fh.Size > maxBytes {
			http.Error(w, fmt.Sprintf("%s exceeds %d MB limit", fh.Filename, s.cfg.MaxFileSizeMB), http.StatusBadRequest)
			// clean up already-written files
			for _, f := range uploaded {
				os.Remove(filepath.Join(s.cfg.StorageDir, f.StoredName))
			}
			return
		}

		src, err := fh.Open()
		if err != nil {
			http.Error(w, "failed to read upload", http.StatusInternalServerError)
			return
		}

		safe := sanitizeFilename(fh.Filename)
		stored := randomID(8) + "_" + safe
		dst, err := os.Create(filepath.Join(s.cfg.StorageDir, stored))
		if err != nil {
			src.Close()
			http.Error(w, "failed to create file", http.StatusInternalServerError)
			return
		}

		n, err := io.Copy(dst, src)
		src.Close()
		dst.Close()
		if err != nil {
			os.Remove(filepath.Join(s.cfg.StorageDir, stored))
			http.Error(w, "failed to write file", http.StatusInternalServerError)
			return
		}

		uploaded = append(uploaded, FileInfo{
			Name:       fh.Filename,
			Size:       n,
			UploadedAt: time.Now(),
			StoredName: stored,
		})
	}

	share := &Share{
		ID:        randomID(10),
		Files:     uploaded,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Duration(s.cfg.ShareExpiryHours) * time.Hour),
	}
	if err := s.store.add(share); err != nil {
		http.Error(w, "failed to save share", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toShareOut(share, baseURL(r)))
}

// handleUploadSingle accepts a raw request body as one file, for simple
// scripting: curl --data-binary @file "http://host/api/upload/file?name=file.txt"
func (s *server) handleUploadSingle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := ipOf(r)
	if !s.rl.allow(ip+"_up", 10.0/60.0, 10) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		name = r.Header.Get("X-Filename")
	}
	if name == "" {
		http.Error(w, "filename required: pass ?name=... or X-Filename header", http.StatusBadRequest)
		return
	}

	maxBytes := int64(s.cfg.MaxFileSizeMB) * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1024)

	if err := os.MkdirAll(s.cfg.StorageDir, 0755); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	safe := sanitizeFilename(name)
	stored := randomID(8) + "_" + safe
	dst, err := os.Create(filepath.Join(s.cfg.StorageDir, stored))
	if err != nil {
		http.Error(w, "failed to create file", http.StatusInternalServerError)
		return
	}

	n, err := io.Copy(dst, r.Body)
	dst.Close()
	if err != nil {
		os.Remove(filepath.Join(s.cfg.StorageDir, stored))
		http.Error(w, fmt.Sprintf("upload failed (over %d MB limit?)", s.cfg.MaxFileSizeMB), http.StatusBadRequest)
		return
	}

	share := &Share{
		ID: randomID(10),
		Files: []FileInfo{{
			Name:       name,
			Size:       n,
			UploadedAt: time.Now(),
			StoredName: stored,
		}},
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Duration(s.cfg.ShareExpiryHours) * time.Hour),
	}
	if err := s.store.add(share); err != nil {
		http.Error(w, "failed to save share", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toShareOut(share, baseURL(r)))
}

func (s *server) handleShares(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	base := baseURL(r)
	shares := s.store.list()
	out := make([]ShareOut, 0, len(shares))
	for _, sh := range shares {
		out = append(out, toShareOut(sh, base))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *server) handleShareOp(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/shares/")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		share, ok := s.store.delete(id)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		for _, f := range share.Files {
			os.Remove(filepath.Join(s.cfg.StorageDir, f.StoredName))
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		share, ok := s.store.get(id)
		if !ok || time.Now().After(share.ExpiresAt) {
			http.Error(w, "not found or expired", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toShareOut(share, baseURL(r)))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	// path: /d/{shareID}/{filename}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/d/"), "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	shareID, filename := parts[0], parts[1]

	share, ok := s.store.get(shareID)
	if !ok || time.Now().After(share.ExpiresAt) {
		http.Error(w, "share not found or expired", http.StatusNotFound)
		return
	}

	var found *FileInfo
	for i := range share.Files {
		if share.Files[i].Name == filename {
			found = &share.Files[i]
			break
		}
	}
	if found == nil {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(filepath.Join(s.cfg.StorageDir, found.StoredName))
	if err != nil {
		http.Error(w, "file not found on disk", http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, found.Name))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(found.Size, 10))
	io.Copy(w, f)
}

func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		type resp struct {
			Config        *Config `json:"config"`
			IP            string  `json:"ip"`
			DiskUsageBytes int64  `json:"disk_usage_bytes"`
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp{
			Config:        s.cfg,
			IP:            localIP(),
			DiskUsageBytes: dirSize(s.cfg.StorageDir),
		})

	case http.MethodPost:
		var nc Config
		if err := json.NewDecoder(r.Body).Decode(&nc); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if nc.Port < 1 || nc.Port > 65535 {
			nc.Port = s.cfg.Port
		}
		if nc.MaxFileSizeMB < 1 {
			nc.MaxFileSizeMB = 1
		}
		if nc.ShareExpiryHours < 1 {
			nc.ShareExpiryHours = 1
		}
		if nc.StorageDir == "" {
			nc.StorageDir = s.cfg.StorageDir
		}
		*s.cfg = nc
		data, _ := json.MarshalIndent(s.cfg, "", "  ")
		os.WriteFile(s.cfgPath, data, 0644)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.cfg)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *server) handleLinks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := ipOf(r)
	if !s.rl.allow(ip+"_link", 10.0/60.0, 10) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	var body struct {
		Links []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"links"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var links []LinkInfo
	for _, l := range body.Links {
		u := strings.TrimSpace(l.URL)
		if u == "" {
			continue
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			http.Error(w, "invalid URL (must start with http:// or https://): "+u, http.StatusBadRequest)
			return
		}
		links = append(links, LinkInfo{URL: u, Title: strings.TrimSpace(l.Title)})
	}
	if len(links) == 0 {
		http.Error(w, "no valid links provided", http.StatusBadRequest)
		return
	}

	share := &Share{
		ID:        randomID(10),
		Links:     links,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Duration(s.cfg.ShareExpiryHours) * time.Hour),
	}
	if err := s.store.add(share); err != nil {
		http.Error(w, "failed to save share", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toShareOut(share, baseURL(r)))
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := defaultConfig
			out, _ := json.MarshalIndent(cfg, "", "  ")
			os.WriteFile(path, out, 0644)
			return &cfg, nil
		}
		return nil, err
	}
	cfg := defaultConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func main() {
	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := newStore("shares.json")
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	if err := os.MkdirAll(cfg.StorageDir, 0755); err != nil {
		log.Fatalf("storage dir: %v", err)
	}

	srv := newServer(cfg, "config.json", store)

	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if n := store.cleanup(cfg.StorageDir); n > 0 {
				log.Printf("cleaned up %d expired shares", n)
			}
		}
	}()

	ip := localIP()
	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("FileShare running:")
	log.Printf("  Local:   http://localhost:%d", cfg.Port)
	log.Printf("  Network: http://%s:%d", ip, cfg.Port)

	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      srv,
		ReadTimeout:  10 * time.Minute, // large for file uploads
		WriteTimeout: 10 * time.Minute,
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
}
