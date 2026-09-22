// Package mcp serves skills over the Model Context Protocol.
//
// It implements the MCP Skills Extension (SEP-2640,
// "io.modelcontextprotocol/skills"): skills/list, skills/get,
// resources/list, resources/read and resources/directory/read, with skills
// addressed as skill://<namespace>/<name>/<file>. The endpoint is
// dual-era: it serves the stateless 2026-07-28 revision (per-request _meta,
// server/discover) and the legacy initialize handshake that most clients
// still use. Because no mainstream client consumes the extension yet, it
// also exposes three plain tools (list_skills, get_skill, read_skill_file)
// that any MCP client can call today.
//
// Every call runs as the authenticated principal, so MCP clients see
// exactly what the policy lets them fetch, and every content read is audited.
package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mthamil107/skills-gateway/internal/auth"
	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/registry"
	"github.com/mthamil107/skills-gateway/internal/reqid"
	"github.com/mthamil107/skills-gateway/internal/skill"
	"github.com/mthamil107/skills-gateway/internal/store"
)

const (
	// ExtensionID is the SEP-2640 extension identifier.
	ExtensionID = "io.modelcontextprotocol/skills"

	modernVersion = "2026-07-28"
	metaVersion   = "io.modelcontextprotocol/protocolVersion"
	metaClientCap = "io.modelcontextprotocol/clientCapabilities"
	metaServer    = "io.modelcontextprotocol/serverInfo"

	// Results depend on the caller's identity, so they are private.
	cacheScope = "private"
	ttlMs      = 60000

	pageSize = 50
)

var legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// JSON-RPC and MCP error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeLegacyNotFound = -32002
	codeHeaderMismatch = -32020
	codeUnsupportedVer = -32022
)

// Handler is the MCP endpoint.
type Handler struct {
	reg     *registry.Service
	version string
	// AllowedOrigins lists browser origins permitted to call the endpoint.
	// Requests without an Origin header (non-browser clients) are allowed.
	AllowedOrigins []string
}

// NewHandler returns an MCP handler backed by reg.
func NewHandler(reg *registry.Service, version string) *Handler {
	return &Handler{reg: reg, version: version}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

func errf(code int, format string, a ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// call carries per-request state.
type call struct {
	ctx    context.Context
	p      auth.Principal
	meta   registry.Meta
	modern bool
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "the MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !h.originAllowed(o, r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeRPC(w, http.StatusBadRequest, nil, nil, errf(codeParse, "cannot read body"))
		return
	}
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
		writeRPC(w, http.StatusBadRequest, nil, nil, errf(codeInvalidRequest, "batch requests are not supported"))
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, http.StatusBadRequest, nil, nil, errf(codeParse, "invalid JSON"))
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, http.StatusBadRequest, req.ID, nil, errf(codeInvalidRequest, "not a JSON-RPC 2.0 request"))
		return
	}
	isNotification := len(req.ID) == 0
	if !isNotification && string(req.ID) == "null" {
		writeRPC(w, http.StatusBadRequest, nil, nil, errf(codeInvalidRequest, "request id must not be null"))
		return
	}
	if m := r.Header.Get("Mcp-Method"); m != "" && m != req.Method {
		writeRPC(w, http.StatusBadRequest, req.ID, nil, errf(codeHeaderMismatch, "Mcp-Method header %q does not match method %q", m, req.Method))
		return
	}

	// Era detection: modern requests carry the protocol version in _meta.
	var env struct {
		Meta map[string]json.RawMessage `json:"_meta"`
		URI  string                     `json:"uri"`
		Name string                     `json:"name"`
	}
	_ = json.Unmarshal(req.Params, &env)
	c := &call{ctx: r.Context(), meta: registry.Meta{RemoteAddr: r.RemoteAddr, RequestID: reqid.From(r.Context())}}
	c.p, _ = auth.FromContext(r.Context())
	if raw, ok := env.Meta[metaVersion]; ok {
		c.modern = true
		var v string
		json.Unmarshal(raw, &v)
		if v != modernVersion {
			writeRPC(w, http.StatusBadRequest, req.ID, nil, &rpcError{Code: codeUnsupportedVer, Message: "Unsupported protocol version",
				Data: map[string]any{"supported": append([]string{modernVersion}, legacyVersions...), "requested": v}})
			return
		}
		if hv := r.Header.Get("MCP-Protocol-Version"); hv != v {
			writeRPC(w, http.StatusBadRequest, req.ID, nil, errf(codeHeaderMismatch, "MCP-Protocol-Version header %q does not match _meta protocol version %q", hv, v))
			return
		}
		if _, ok := env.Meta[metaClientCap]; !ok {
			writeRPC(w, http.StatusBadRequest, req.ID, nil, errf(codeInvalidParams, "_meta is missing %s", metaClientCap))
			return
		}
		if n := r.Header.Get("Mcp-Name"); n != "" {
			want := env.URI
			if req.Method == "tools/call" || req.Method == "prompts/get" {
				want = env.Name
			}
			if want != "" && decodeHeader(n) != want {
				writeRPC(w, http.StatusBadRequest, req.ID, nil, errf(codeHeaderMismatch, "Mcp-Name header does not match the request"))
				return
			}
		}
	} else if hv := r.Header.Get("MCP-Protocol-Version"); hv == modernVersion && req.Method != "initialize" {
		writeRPC(w, http.StatusBadRequest, req.ID, nil, errf(codeInvalidParams, "protocol %s requires _meta[%q] in params", modernVersion, metaVersion))
		return
	} else if hv != "" && !slices.Contains(legacyVersions, hv) && req.Method != "initialize" {
		writeRPC(w, http.StatusBadRequest, req.ID, nil, &rpcError{Code: codeUnsupportedVer, Message: "Unsupported protocol version",
			Data: map[string]any{"supported": append([]string{modernVersion}, legacyVersions...), "requested": hv}})
		return
	}

	if isNotification {
		// notifications/initialized and friends: accept and ignore.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rerr := h.dispatch(c, req.Method, req.Params)
	if rerr != nil {
		status := http.StatusOK
		if rerr.Code == codeMethodNotFound && c.modern {
			status = http.StatusNotFound
		}
		writeRPC(w, status, req.ID, nil, rerr)
		return
	}
	if c.modern {
		result["resultType"] = "complete"
		meta, _ := result["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta[metaServer] = h.serverInfo()
		result["_meta"] = meta
	}
	writeRPC(w, http.StatusOK, req.ID, result, nil)
}

func decodeHeader(v string) string {
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") {
		if b, err := base64.StdEncoding.DecodeString(v[len("=?base64?") : len(v)-2]); err == nil {
			return string(b)
		}
	}
	return v
}

func (h *Handler) originAllowed(origin string, r *http.Request) bool {
	if slices.Contains(h.AllowedOrigins, origin) {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func writeRPC(w http.ResponseWriter, status int, id json.RawMessage, result any, e *rpcError) {
	resp := map[string]any{"jsonrpc": "2.0"}
	if len(id) > 0 {
		resp["id"] = id
	}
	if e != nil {
		resp["error"] = e
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) serverInfo() map[string]any {
	return map[string]any{"name": "skills-gateway", "version": h.version}
}

func (h *Handler) capabilities() map[string]any {
	return map[string]any{
		"resources":  map[string]any{},
		"tools":      map[string]any{},
		"extensions": map[string]any{ExtensionID: map[string]any{"directoryRead": true}},
	}
}

const instructions = "Skills Gateway serves Agent Skills (SKILL.md) you are authorized to use. " +
	"Call list_skills to discover skills, get_skill to load one, and read_skill_file for its supporting files."

func cacheable(c *call, m map[string]any) map[string]any {
	if c.modern {
		m["ttlMs"] = ttlMs
		m["cacheScope"] = cacheScope
	}
	return m
}

func (h *Handler) dispatch(c *call, method string, params json.RawMessage) (map[string]any, *rpcError) {
	switch method {
	case "server/discover":
		return cacheable(c, map[string]any{
			"supportedVersions": []string{modernVersion},
			"capabilities":      h.capabilities(),
			"instructions":      instructions,
		}), nil
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(params, &p)
		v := p.ProtocolVersion
		if !slices.Contains(legacyVersions, v) {
			v = legacyVersions[0]
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    h.capabilities(),
			"serverInfo":      h.serverInfo(),
			"instructions":    instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "skills/list":
		return h.skillsList(c, params)
	case "skills/get":
		return h.skillsGet(c, params)
	case "resources/list":
		return h.resourcesList(c, params)
	case "resources/templates/list":
		return cacheable(c, map[string]any{"resourceTemplates": []any{}}), nil
	case "resources/read":
		return h.resourcesRead(c, params)
	case "resources/directory/read":
		return h.directoryRead(c, params)
	case "tools/list":
		return cacheable(c, map[string]any{"tools": tools}), nil
	case "tools/call":
		return h.toolsCall(c, params)
	}
	return nil, errf(codeMethodNotFound, "method %q not found", method)
}

// ---- skill loading ------------------------------------------------------

// ref is a parsed skill:// URI.
type ref struct {
	ns, name, file string // file is "" for the skill root directory
}

func parseURI(uri string) (ref, bool) {
	rest, ok := strings.CutPrefix(uri, "skill://")
	if !ok {
		return ref{}, false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || !skill.ValidName(parts[0]) || !skill.ValidName(parts[1]) {
		return ref{}, false
	}
	r := ref{ns: parts[0], name: parts[1]}
	if len(parts) == 3 {
		p, err := bundle.CleanPath(parts[2])
		if err != nil {
			return ref{}, false
		}
		r.file = p
	}
	return r, true
}

func skillURI(ns, name string) string { return "skill://" + ns + "/" + name }

// loaded is the latest visible version of a skill. files holds the
// manifest only; content is read on demand.
type loaded struct {
	v     store.Version
	files []bundle.File
	front map[string]any
}

func (h *Handler) load(c *call, ns, name, what string) (*loaded, error) {
	v, err := h.reg.Get(c.ctx, c.p, c.meta, ns, name, "latest", what)
	if err != nil {
		return nil, err
	}
	l := &loaded{v: v, files: v.Files}
	if md, err := h.content(c, l, "SKILL.md"); err == nil {
		l.front, _ = skill.RawFrontmatter(md)
	}
	if l.front == nil {
		l.front = map[string]any{"name": v.Name, "description": v.Description}
	}
	return l, nil
}

func (h *Handler) content(c *call, l *loaded, p string) ([]byte, error) {
	return h.reg.Store.FileContent(c.ctx, l.v.Namespace, l.v.Name, l.v.Version, p)
}

func (l *loaded) entry() map[string]any {
	base := skillURI(l.v.Namespace, l.v.Name)
	res := make([]map[string]any, 0, len(l.files))
	for _, f := range l.files {
		res = append(res, map[string]any{"uri": base + "/" + f.Path, "digest": "sha256:" + f.SHA256, "size": f.Size})
	}
	return map[string]any{"uri": base + "/SKILL.md", "frontmatter": l.front, "resources": res}
}

func notFound(uri string) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: "No skill is served at " + uri, Data: map[string]any{"uri": uri}}
}

func internal(err error) *rpcError { return errf(codeInternal, "internal error: %v", err) }

func loadErr(err error, uri string) *rpcError {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, registry.ErrInvalid) {
		return notFound(uri)
	}
	return internal(err)
}

type pageParams struct {
	Cursor string `json:"cursor"`
}

// visible returns one page of skills the caller may fetch.
func (h *Handler) visible(c *call, ns, cursor string, limit int) (registry.Page, error) {
	return h.reg.List(c.ctx, c.p, store.ListFilter{Namespace: ns}, limit, cursor)
}

func (h *Handler) skillsList(c *call, params json.RawMessage) (map[string]any, *rpcError) {
	var p pageParams
	json.Unmarshal(params, &p)
	page, err := h.visible(c, "", p.Cursor, pageSize)
	if err != nil {
		return nil, internal(err)
	}
	out := make([]map[string]any, 0, len(page.Items))
	for _, v := range page.Items {
		l, err := h.load(c, v.Namespace, v.Name, "")
		if err != nil {
			continue // raced with a deprecation or policy change
		}
		out = append(out, l.entry())
	}
	res := map[string]any{"skills": out}
	if page.NextCursor != "" {
		res["nextCursor"] = page.NextCursor
	}
	return cacheable(c, res), nil
}

func (h *Handler) skillsGet(c *call, params json.RawMessage) (map[string]any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	json.Unmarshal(params, &p)
	r, ok := parseURI(p.URI)
	if !ok || r.file != "SKILL.md" {
		return nil, notFound(p.URI)
	}
	l, err := h.load(c, r.ns, r.name, "mcp:skills/get")
	if err != nil {
		return nil, loadErr(err, p.URI)
	}
	return cacheable(c, map[string]any{"skill": l.entry()}), nil
}

func mimeOf(p string) string {
	if strings.HasSuffix(p, ".md") {
		return "text/markdown"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func (l *loaded) resource(f bundle.File) map[string]any {
	r := map[string]any{
		"uri": skillURI(l.v.Namespace, l.v.Name) + "/" + f.Path, "name": path.Base(f.Path),
		"mimeType": mimeOf(f.Path), "size": f.Size,
	}
	if f.Path == "SKILL.md" {
		r["name"] = l.v.Name
		r["description"] = l.v.Description
	}
	return r
}

func (h *Handler) resourcesList(c *call, params json.RawMessage) (map[string]any, *rpcError) {
	var p pageParams
	json.Unmarshal(params, &p)
	page, err := h.visible(c, "", p.Cursor, pageSize)
	if err != nil {
		return nil, internal(err)
	}
	out := []map[string]any{}
	for _, v := range page.Items {
		l, err := h.load(c, v.Namespace, v.Name, "")
		if err != nil {
			continue
		}
		for _, f := range l.files {
			out = append(out, l.resource(f))
		}
	}
	res := map[string]any{"resources": out}
	if page.NextCursor != "" {
		res["nextCursor"] = page.NextCursor
	}
	return cacheable(c, res), nil
}

func (h *Handler) resourcesRead(c *call, params json.RawMessage) (map[string]any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	json.Unmarshal(params, &p)
	missing := func() *rpcError {
		e := &rpcError{Code: codeInvalidParams, Message: "Resource not found", Data: map[string]any{"uri": p.URI}}
		if !c.modern {
			e.Code = codeLegacyNotFound
		}
		return e
	}
	r, ok := parseURI(p.URI)
	if !ok || r.file == "" {
		return nil, missing()
	}
	l, err := h.load(c, r.ns, r.name, "mcp:file:"+r.file)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, missing()
		}
		return nil, internal(err)
	}
	for _, f := range l.files {
		if f.Path == r.file {
			data, err := h.content(c, l, f.Path)
			if err != nil {
				return nil, internal(err)
			}
			content := map[string]any{"uri": p.URI, "mimeType": mimeOf(f.Path)}
			if utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
				content["text"] = string(data)
			} else {
				content["blob"] = base64.StdEncoding.EncodeToString(data)
			}
			return cacheable(c, map[string]any{"contents": []any{content}}), nil
		}
	}
	return nil, missing()
}

func (h *Handler) directoryRead(c *call, params json.RawMessage) (map[string]any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	json.Unmarshal(params, &p)
	notDir := errf(codeInvalidParams, "%s is not a directory resource", p.URI)

	// Namespace level: skill://<ns> lists the skills in it.
	if ns, ok := strings.CutPrefix(p.URI, "skill://"); ok && skill.ValidName(ns) {
		out := []map[string]any{}
		cursor := ""
		for {
			page, err := h.visible(c, ns, cursor, 200)
			if err != nil {
				return nil, internal(err)
			}
			for _, v := range page.Items {
				out = append(out, map[string]any{"uri": skillURI(v.Namespace, v.Name), "name": v.Name, "mimeType": "inode/directory"})
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if len(out) == 0 {
			return nil, notDir
		}
		return map[string]any{"resources": out}, nil
	}

	r, ok := parseURI(p.URI)
	if !ok {
		return nil, notDir
	}
	l, err := h.load(c, r.ns, r.name, "")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notDir
		}
		return nil, internal(err)
	}
	prefix := ""
	if r.file != "" {
		prefix = r.file + "/"
	}
	isDir := r.file == ""
	children := map[string]map[string]any{}
	for _, f := range l.files {
		rest, ok := strings.CutPrefix(f.Path, prefix)
		if !ok {
			continue
		}
		isDir = true
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			d := rest[:i]
			children[d] = map[string]any{"uri": skillURI(r.ns, r.name) + "/" + prefix + d, "name": d, "mimeType": "inode/directory"}
		} else {
			children[rest] = l.resource(f)
		}
	}
	if !isDir {
		return nil, notDir
	}
	keys := make([]string, 0, len(children))
	for k := range children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, children[k])
	}
	return map[string]any{"resources": out}, nil
}
