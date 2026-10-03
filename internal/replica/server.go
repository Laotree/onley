package replica

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"onley/internal/db"
)

// maxIngestMemory is how much of an upload is held in memory before the rest
// goes to a temporary file. It used to be 512 MB, which is a large amount of RAM
// to spend on a single request. onley exists to move large files around, so the
// ordinary case now lands on disk.
const maxIngestMemory = 32 << 20

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

// isMD5Hex reports whether s is a 32 character lowercase hex digest.
//
// Lowercase only, because the value becomes a directory name: accepting "AB" as
// well as "ab" would put one digest in two places.
func isMD5Hex(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
	// The file part arrives as a readable stream either way; maxIngestMemory only
	// decides whether it is backed by memory or by a temporary file.
	if err := r.ParseMultipartForm(maxIngestMemory); err != nil {
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
	if !isMD5Hex(md5sum) {
		// The store layout is derived from this value, and the comparison below
		// is only meaningful against a real digest, so the format is checked
		// rather than merely the length.
		http.Error(w, "missing or invalid md5: want 32 lowercase hex characters, as produced by the replica", http.StatusBadRequest)
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

	// Hash what actually arrived rather than believing the md5 field. The two can
	// disagree without anybody lying: the replica hashes a file, the file changes,
	// and the upload sends the new bytes under the old digest.
	//
	// The consequence of not checking is not a misplaced file. Check answers
	// "do you have this digest" from the index, so storing content under a digest
	// it does not have tells every other replica that this content is backed up
	// here, and they delete their copy. It would be backed up nowhere.
	hasher := md5.New()
	size, err := io.Copy(io.MultiWriter(out, hasher), file)
	if err != nil {
		out.Close()
		os.Remove(destPath)
		http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Close before reading back, so the bytes are on disk before anything can
	// observe them.
	if err := out.Close(); err != nil {
		os.Remove(destPath)
		http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if got := fmt.Sprintf("%x", hasher.Sum(nil)); got != md5sum {
		// Leave nothing behind. A file whose content does not match its address
		// is worse than an absent one: the next replica to ask about this digest
		// finds it.
		os.Remove(destPath)
		http.Error(w, fmt.Sprintf("md5 mismatch: the upload declares %s but hashes to %s, so it is not the file the replica indexed", md5sum, got), http.StatusBadRequest)
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
		os.Remove(destPath)
		http.Error(w, "index: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, ingestResp{OK: true, Path: destPath})
}
