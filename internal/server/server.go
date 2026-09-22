// Package server exposes the registry over HTTP: the REST API under /v1
// and the MCP endpoint at /mcp.
package server

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mthamil107/skills-gateway/internal/auth"
	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/registry"
	"github.com/mthamil107/skills-gateway/internal/skill"
	"github.com/mthamil107/skills-gateway/internal/store"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

//go:embed openapi.yaml
var openapiSpec []byte

// Server wires HTTP handlers to the registry.
type Server struct {
	Reg         *registry.Service
	Auth        auth.Authenticator
	Translators translate.Registry
	Log         *slog.Logger
	MaxBody     int64 // maximum compressed upload size
	// MCP, if set, is mounted at /mcp behind the same authentication.
	MCP http.Handler
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	if s.MaxBody == 0 {
		s.MaxBody = 10 << 20
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(openapiSpec)
	})

	api := http.NewServeMux()
	api.HandleFunc("GET /v1/whoami", s.whoami)
	api.HandleFunc("GET /v1/formats", s.formats)
	api.HandleFunc("GET /v1/skills", s.list)
	api.HandleFunc("GET /v1/skills/{ns}/{name}", s.versions)
	api.HandleFunc("GET /v1/skills/{ns}/{name}/versions/{ver}", s.getVersion)
	api.HandleFunc("PUT /v1/skills/{ns}/{name}/versions/{ver}", s.publish)
	api.HandleFunc("GET /v1/skills/{ns}/{name}/versions/{ver}/bundle", s.getBundle)
	api.HandleFunc("GET /v1/skills/{ns}/{name}/versions/{ver}/files/{path...}", s.getFile)
	api.HandleFunc("GET /v1/skills/{ns}/{name}/versions/{ver}/translate", s.translate)
	api.HandleFunc("POST /v1/skills/{ns}/{name}/versions/{ver}/deprecate", s.deprecate)
	api.HandleFunc("GET /v1/audit", s.exportAudit)
	mux.Handle("/v1/", s.authenticate(api))
	if s.MCP != nil {
		mux.Handle("/mcp", s.authenticate(s.MCP))
	}
	return s.requestID(mux)
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 8)
		rand.Read(b)
		id := hex.EncodeToString(b)
		w.Header().Set("X-Request-Id", id)
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r.WithContext(contextWith(r, id)))
		s.Log.Info("request", "id", id, "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := ""
		if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			bearer = strings.TrimSpace(h[7:])
		}
		p, err := s.Auth.Authenticate(r.Context(), bearer)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="skills-gateway"`)
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

func principal(r *http.Request) auth.Principal {
	p, _ := auth.FromContext(r.Context())
	return p
}

func meta(r *http.Request) registry.Meta {
	return registry.Meta{RequestID: RequestID(r.Context()), RemoteAddr: r.RemoteAddr}
}

// Error envelope: {"error":{"code":..., "message":..., "request_id":...}}
type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg, RequestID: RequestID(r.Context())}})
}

// fail maps domain errors onto HTTP responses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds "+strconv.FormatInt(s.MaxBody, 10)+" bytes")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "no such skill or version")
	case errors.Is(err, store.ErrVersionExists):
		writeError(w, r, http.StatusConflict, "version_exists", "this version is already published and cannot be changed; publish a new version")
	case errors.Is(err, registry.ErrForbidden):
		writeError(w, r, http.StatusForbidden, "forbidden", "policy does not allow this action")
	case errors.Is(err, registry.ErrDigestMismatch):
		writeError(w, r, http.StatusBadRequest, "digest_mismatch", err.Error())
	case errors.Is(err, bundle.ErrInvalid), errors.Is(err, registry.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		s.Log.Error("internal error", "id", RequestID(r.Context()), "err", err)
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error")
	}
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, principal(r))
}

func (s *Server) formats(w http.ResponseWriter, r *http.Request) {
	type f struct {
		Format      string `json:"format"`
		Description string `json:"description"`
	}
	var out []f
	for _, id := range s.Translators.Formats() {
		out = append(out, f{id, s.Translators[id].Description()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"formats": out})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	page, err := s.Reg.List(r.Context(), principal(r), store.ListFilter{
		Namespace: q.Get("namespace"), Query: q.Get("q"), Tag: q.Get("tag"),
	}, limit, q.Get("cursor"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if page.Items == nil {
		page.Items = []store.Version{}
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) versions(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Reg.Versions(r.Context(), principal(r), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": r.PathValue("ns"), "name": r.PathValue("name"), "versions": vs})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, what string) (store.Version, bool) {
	v, err := s.Reg.Get(r.Context(), principal(r), meta(r), r.PathValue("ns"), r.PathValue("name"), r.PathValue("ver"), what)
	if err != nil {
		s.fail(w, r, err)
		return v, false
	}
	return v, true
}

// notModified sets the ETag and reports whether the client copy is current.
func notModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	w.Header().Set("ETag", `"`+etag+`"`)
	for _, t := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if strings.Trim(strings.TrimSpace(t), `"`) == etag {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	v, ok := s.get(w, r, "")
	if !ok {
		return
	}
	// Content is immutable but status is not, so the validator must change
	// with it or a cached client never sees a deprecation.
	etag := v.Digest
	if v.Status != store.Published {
		etag += ":" + string(v.Status)
	}
	if notModified(w, r, etag) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getBundle(w http.ResponseWriter, r *http.Request) {
	v, ok := s.get(w, r, "bundle")
	if !ok || notModified(w, r, v.Digest) {
		return
	}
	files, err := s.Reg.Files(r.Context(), v)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data, err := bundle.Encode(files)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("X-Bundle-Digest", v.Digest)
	w.Header().Set("Content-Disposition", `attachment; filename="`+v.Name+"-"+v.Version+`.tar.gz"`)
	w.Write(data)
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	p, err := bundle.CleanPath(r.PathValue("path"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, ok := s.get(w, r, "file:"+p)
	if !ok {
		return
	}
	var sum string
	for _, f := range v.Files {
		if f.Path == p {
			sum = f.SHA256
		}
	}
	if sum == "" {
		s.fail(w, r, store.ErrNotFound)
		return
	}
	if notModified(w, r, "sha256:"+sum) {
		return
	}
	data, err := s.Reg.Store.FileContent(r.Context(), v.Namespace, v.Name, v.Version, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ct := mime.TypeByExtension(pathExt(p))
	if ct == "" || strings.HasSuffix(p, ".md") {
		ct = "text/plain; charset=utf-8"
		if !isText(data) {
			ct = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Write(data)
}

func pathExt(p string) string {
	if i := strings.LastIndexByte(p, '.'); i > strings.LastIndexByte(p, '/') {
		return p[i:]
	}
	return ""
}

func isText(b []byte) bool { return !bytes.ContainsRune(b, 0) }

func (s *Server) translate(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "claude"
	}
	tr, ok := s.Translators[format]
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "unknown format "+strconv.Quote(format)+"; see GET /v1/formats")
		return
	}
	v, ok := s.get(w, r, "translate:"+format)
	if !ok {
		return
	}
	files, err := s.Reg.Files(r.Context(), v)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	md, _ := bundle.FromFiles(files).File("SKILL.md")
	sk, err := skill.Parse(md.Content)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := tr.Translate(sk, files)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"namespace": v.Namespace, "name": v.Name, "version": v.Version,
		"digest": v.Digest, "result": res,
	})
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	// Read the whole (bounded) upload first so an oversized body is always
	// reported as 413, never as a corrupt archive.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.MaxBody))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Reg.Publish(r.Context(), principal(r), meta(r),
		r.PathValue("ns"), r.PathValue("name"), r.PathValue("ver"), bytes.NewReader(body), r.Header.Get("X-Bundle-Digest"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/skills/"+v.Namespace+"/"+v.Name+"/versions/"+v.Version)
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) deprecate(w http.ResponseWriter, r *http.Request) {
	v, err := s.Reg.Deprecate(r.Context(), principal(r), meta(r), r.PathValue("ns"), r.PathValue("name"), r.PathValue("ver"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) exportAudit(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if q := r.URL.Query().Get("since"); q != "" {
		t, err := time.Parse(time.RFC3339, q)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "since must be RFC 3339")
			return
		}
		since = t
	}
	// Authorize before writing any headers so a denial is a clean 403.
	var buf bytes.Buffer
	if err := s.Reg.ExportAudit(r.Context(), principal(r), since, &buf); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Write(buf.Bytes())
}
