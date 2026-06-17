package replica

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"onley/internal/db"
)

// Server implements the master HTTP API for the replica feature.
type Server struct {
	store    *db.DB
	storeDir string
}

// NewServer creates a master server that answers queries from replicas and
// accepts file uploads. storeDir is where ingested files are written using
// content-addressed paths (<storeDir>/<md5[0:2]>/<md5[2:]>/<filename>).
func NewServer(store *db.DB, storeDir string) *Server {
	return &Server{store: store, storeDir: storeDir}
}

// Handler returns the http.Handler for all replica API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/check", s.handleCheck)
	mux.HandleFunc("POST /v1/ingest", s.handleIngest)
	return mux
}

// checkResp is the JSON body for GET /v1/check.
type checkResp struct {
	Found bool     `json:"found"`
	Paths []string `json:"paths,omitempty"`
}

// ingestResp is the JSON body for a successful POST /v1/ingest.
type ingestResp struct {
	OK   bool   `json:"ok"`
	Path string `json:"path"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	md5sum := r.URL.Query().Get("md5")
	if md5sum == "" {
		http.Error(w, "missing md5 parameter", http.StatusBadRequest)
		return
	}

	files, err := s.store.FindByMD5(md5sum)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := checkResp{Found: len(files) > 0}
	for _, f := range files {
		resp.Paths = append(resp.Paths, f.Path)
	}
	writeJSON(w, resp)
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(512 << 20); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	md5sum := r.FormValue("md5")
	if len(md5sum) < 4 {
		http.Error(w, "missing or invalid md5", http.StatusBadRequest)
		return
	}

	// Content-addressed path avoids name collisions across replicas.
	destDir := filepath.Join(s.storeDir, md5sum[:2], md5sum[2:])
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		http.Error(w, "mkdir: "+err.Error(), http.StatusInternalServerError)
		return
	}

	destPath := filepath.Join(destDir, header.Filename)
	out, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "create: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer out.Close()

	size, err := io.Copy(out, file)
	if err != nil {
		http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rec := db.FileRecord{
		Path:  destPath,
		Name:  header.Filename,
		Size:  size,
		MD5:   md5sum,
		Mtime: 0,
	}
	if err := s.store.Upsert(rec); err != nil {
		http.Error(w, "index: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, ingestResp{OK: true, Path: destPath})
}
