package mcp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/gatewaytest"
	"github.com/mthamil107/skills-gateway/internal/mcp"
)

const (
	modernVersion = "2026-07-28"
	invoice       = "# Invoice\n\nCustomer:\nAmount:\n"
	invoiceDigest = "sha256:61f4ea6d2c75fde1b4977219e7e3107d491c3c26aefb6686e84d6281c088d9ee"
	pdfMD         = "---\nname: pdf-processing\ndescription: Extract, fill, and assemble PDF documents\nlicense: Apache-2.0\nmetadata:\n  owner: docs\nagents:\n  cursor:\n    globs: \"**/*.pdf\"\n---\n\n# PDF processing\n\nChoose the matching template from `templates/`.\n"
)

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type rpcResp struct {
	Status int
	Body   map[string]any
	Raw    []byte
	Header http.Header
}

func (r rpcResp) result(t *testing.T) map[string]any {
	t.Helper()
	res, ok := r.Body["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result (status %d): %s", r.Status, r.Raw)
	}
	return res
}

func (r rpcResp) errCode(t *testing.T) int {
	t.Helper()
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error (status %d): %s", r.Status, r.Raw)
	}
	return int(e["code"].(float64))
}

func (r rpcResp) errMessage() string {
	e, _ := r.Body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

// post sends one JSON-RPC message with token; extra headers may be nil.
func post(t *testing.T, g *gatewaytest.Gateway, token string, hdr map[string]string, msg any) rpcResp {
	t.Helper()
	var body []byte
	switch m := msg.(type) {
	case string:
		body = []byte(m)
	default:
		body, _ = json.Marshal(m)
	}
	h := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
	for k, v := range hdr {
		h[k] = v
	}
	resp := g.Do(t, http.MethodPost, "/mcp", token, bytes.NewReader(body), h)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := rpcResp{Status: resp.StatusCode, Raw: raw, Header: resp.Header}
	if len(bytes.TrimSpace(raw)) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &out.Body); err != nil {
			t.Fatalf("response is not a JSON object: %s", raw)
		}
	}
	return out
}

func legacy(id any, method string, params map[string]any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

// modern builds a 2026-07-28 request with the required _meta.
func modern(id any, method string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    modernVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test", "version": "0"},
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

func modernHdr(method string) map[string]string {
	return map[string]string{"MCP-Protocol-Version": modernVersion, "Mcp-Method": method}
}

// call is the common modern round trip.
func call(t *testing.T, g *gatewaytest.Gateway, token, method string, params map[string]any) rpcResp {
	t.Helper()
	return post(t, g, token, modernHdr(method), modern(1, method, params))
}

func fixture(t *testing.T) *gatewaytest.Gateway {
	t.Helper()
	g := gatewaytest.New(t, gatewaytest.Options{Version: "9.9.9-test"})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "pdf-processing", "1.0.0", gatewaytest.Files("pdf-processing", map[string]string{
		"SKILL.md":                    pdfMD,
		"templates/invoice.md":        invoice,
		"templates/purchase-order.md": "# Purchase order\n\nVendor:\nItems:\n",
		"templates/regional/eu.md":    "EU\n",
		"scripts/extract.py":          "print('x')\n",
		"assets/logo.png":             "\x89PNG\r\n\x1a\n\x00\x00binary",
		"assets/notes.txt":            "plain",
	}))
	g.Publish(t, gatewaytest.PlatformToken, "platform", "git-workflow", "1.0.0", gatewaytest.Files("git-workflow", nil))
	g.Publish(t, gatewaytest.PlatformToken, "platform", "secret-ops", "1.0.0", gatewaytest.Files("secret-ops", nil))
	g.Publish(t, gatewaytest.PaymentsToken, "payments", "refunds", "1.0.0", gatewaytest.Files("refunds", map[string]string{"examples/email.md": "Dear customer,\n"}))
	return g
}

func TestLegacyInitialize(t *testing.T) {
	g := fixture(t)
	r := post(t, g, gatewaytest.ReaderToken, nil, legacy(1, "initialize", map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "c", "version": "1"},
	}))
	if r.Status != 200 {
		t.Fatalf("status %d: %s", r.Status, r.Raw)
	}
	res := r.result(t)
	if res["protocolVersion"] != "2025-11-25" {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["resources"].(map[string]any); !ok {
		t.Errorf("capabilities.resources missing: %v", caps)
	}
	if _, ok := caps["tools"].(map[string]any); !ok {
		t.Errorf("capabilities.tools missing: %v", caps)
	}
	ext, _ := caps["extensions"].(map[string]any)
	skills, ok := ext[mcp.ExtensionID].(map[string]any)
	if !ok {
		t.Fatalf("capabilities.extensions[%s] missing: %v", mcp.ExtensionID, caps)
	}
	if skills["directoryRead"] != true {
		t.Errorf("directoryRead = %v", skills["directoryRead"])
	}
	si, _ := res["serverInfo"].(map[string]any)
	if si["name"] != "skills-gateway" || si["version"] != "9.9.9-test" {
		t.Errorf("serverInfo = %v", si)
	}
	if s, _ := res["instructions"].(string); !strings.Contains(s, "list_skills") {
		t.Errorf("instructions = %q", s)
	}
	if _, ok := res["resultType"]; ok {
		t.Error("legacy result should not carry resultType")
	}
	// Unknown legacy versions negotiate down to the newest supported one; a
	// legacy MCP-Protocol-Version header is accepted on later calls.
	r = post(t, g, gatewaytest.ReaderToken, nil, legacy(2, "initialize", map[string]any{"protocolVersion": "2024-11-05"}))
	if r.result(t)["protocolVersion"] != "2025-11-25" {
		t.Errorf("negotiated %v", r.result(t)["protocolVersion"])
	}
	r = post(t, g, gatewaytest.ReaderToken, map[string]string{"MCP-Protocol-Version": "2025-06-18"}, legacy(3, "ping", nil))
	if r.Status != 200 || len(r.result(t)) != 0 {
		t.Errorf("ping: %d %s", r.Status, r.Raw)
	}
	if r.Body["id"] != float64(3) {
		t.Errorf("id echoed as %v", r.Body["id"])
	}
	r = post(t, g, gatewaytest.ReaderToken, nil, legacy("str-id", "ping", nil))
	if r.Body["id"] != "str-id" {
		t.Errorf("string id echoed as %v", r.Body["id"])
	}
}

func TestTransportRules(t *testing.T) {
	g := fixture(t)
	// Notifications are accepted with 202 and no body.
	r := post(t, g, gatewaytest.ReaderToken, nil, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if r.Status != http.StatusAccepted || len(r.Raw) != 0 {
		t.Errorf("notification: %d %q", r.Status, r.Raw)
	}
	r = post(t, g, gatewaytest.ReaderToken, modernHdr("notifications/cancelled"), map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": modern(0, "x", nil)["params"]})
	if r.Status != http.StatusAccepted {
		t.Errorf("modern notification: %d %q", r.Status, r.Raw)
	}
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		resp := g.Do(t, m, "/mcp", gatewaytest.ReaderToken, nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
			t.Errorf("%s /mcp: %d Allow=%q", m, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
	// Origin validation.
	r = post(t, g, gatewaytest.ReaderToken, map[string]string{"Origin": "https://evil.example"}, legacy(1, "ping", nil))
	if r.Status != http.StatusForbidden {
		t.Errorf("foreign origin: %d %s", r.Status, r.Raw)
	}
	r = post(t, g, gatewaytest.ReaderToken, map[string]string{"Origin": "null"}, legacy(1, "ping", nil))
	if r.Status != http.StatusForbidden {
		t.Errorf("null origin: %d %s", r.Status, r.Raw)
	}
	r = post(t, g, gatewaytest.ReaderToken, map[string]string{"Origin": g.URL}, legacy(1, "ping", nil))
	if r.Status != 200 {
		t.Errorf("same-host origin: %d %s", r.Status, r.Raw)
	}
	// Malformed bodies.
	cases := []struct {
		label string
		body  string
		code  int
	}{
		{"invalid json", "{not json", -32700},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, -32600},
		{"wrong jsonrpc", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, -32600},
		{"no method", `{"jsonrpc":"2.0","id":1}`, -32600},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, -32600},
	}
	for _, c := range cases {
		r := post(t, g, gatewaytest.ReaderToken, nil, c.body)
		if r.Status != http.StatusBadRequest || r.errCode(t) != c.code {
			t.Errorf("%s: %d %s", c.label, r.Status, r.Raw)
		}
	}
	// Mcp-Method must match the body when present (both eras).
	r = post(t, g, gatewaytest.ReaderToken, map[string]string{"Mcp-Method": "tools/list"}, legacy(1, "ping", nil))
	if r.Status != http.StatusBadRequest || r.errCode(t) != -32020 {
		t.Errorf("Mcp-Method mismatch: %d %s", r.Status, r.Raw)
	}
	// Every response is JSON, never SSE.
	r = post(t, g, gatewaytest.ReaderToken, nil, legacy(1, "ping", nil))
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
	}
}

func TestModernDiscoverAndValidation(t *testing.T) {
	g := fixture(t)
	r := call(t, g, gatewaytest.ReaderToken, "server/discover", nil)
	if r.Status != 200 {
		t.Fatalf("discover: %d %s", r.Status, r.Raw)
	}
	res := r.result(t)
	if sv, _ := res["supportedVersions"].([]any); len(sv) != 1 || sv[0] != modernVersion {
		t.Errorf("supportedVersions = %v", res["supportedVersions"])
	}
	if res["ttlMs"] != float64(60000) || res["cacheScope"] != "private" || res["resultType"] != "complete" {
		t.Errorf("cache attrs / resultType: %v %v %v", res["ttlMs"], res["cacheScope"], res["resultType"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["resources"]; !ok {
		t.Errorf("capabilities.resources missing")
	}
	if ext, _ := caps["extensions"].(map[string]any); ext[mcp.ExtensionID] == nil {
		t.Errorf("extension capability missing: %v", caps)
	}
	meta, _ := res["_meta"].(map[string]any)
	if si, _ := meta["io.modelcontextprotocol/serverInfo"].(map[string]any); si["name"] != "skills-gateway" || si["version"] != "9.9.9-test" {
		t.Errorf("_meta.serverInfo = %v", meta)
	}

	// Header/body version mismatch and missing header: 400 + -32020.
	for label, hdr := range map[string]map[string]string{
		"mismatch": {"MCP-Protocol-Version": "2025-11-25"},
		"missing":  {},
		"empty":    {"MCP-Protocol-Version": ""},
	} {
		r := post(t, g, gatewaytest.ReaderToken, hdr, modern(1, "server/discover", nil))
		if r.Status != http.StatusBadRequest || r.errCode(t) != -32020 {
			t.Errorf("%s header: %d %s", label, r.Status, r.Raw)
		}
	}
	// Missing clientCapabilities: 400 + -32602.
	req := modern(1, "server/discover", nil)
	delete(req["params"].(map[string]any)["_meta"].(map[string]any), "io.modelcontextprotocol/clientCapabilities")
	r = post(t, g, gatewaytest.ReaderToken, modernHdr("server/discover"), req)
	if r.Status != http.StatusBadRequest || r.errCode(t) != -32602 {
		t.Errorf("missing clientCapabilities: %d %s", r.Status, r.Raw)
	}
	// Unsupported version in _meta: 400 + -32022 listing supported versions.
	req = modern(1, "server/discover", nil)
	req["params"].(map[string]any)["_meta"].(map[string]any)["io.modelcontextprotocol/protocolVersion"] = "2030-01-01"
	r = post(t, g, gatewaytest.ReaderToken, map[string]string{"MCP-Protocol-Version": "2030-01-01"}, req)
	if r.Status != http.StatusBadRequest || r.errCode(t) != -32022 {
		t.Fatalf("unsupported version: %d %s", r.Status, r.Raw)
	}
	data, _ := r.Body["error"].(map[string]any)["data"].(map[string]any)
	if sup, _ := data["supported"].([]any); len(sup) == 0 || sup[0] != modernVersion || data["requested"] != "2030-01-01" {
		t.Errorf("error.data = %v", data)
	}
	// Legacy request with an unsupported header version is also -32022.
	r = post(t, g, gatewaytest.ReaderToken, map[string]string{"MCP-Protocol-Version": "1999-01-01"}, legacy(1, "ping", nil))
	if r.Status != http.StatusBadRequest || r.errCode(t) != -32022 {
		t.Errorf("legacy unsupported header: %d %s", r.Status, r.Raw)
	}
	// Unknown method: 404 + -32601 (modern), 200 + -32601 (legacy).
	r = call(t, g, gatewaytest.ReaderToken, "skills/frobnicate", nil)
	if r.Status != http.StatusNotFound || r.errCode(t) != -32601 {
		t.Errorf("modern unknown method: %d %s", r.Status, r.Raw)
	}
	r = post(t, g, gatewaytest.ReaderToken, nil, legacy(1, "skills/frobnicate", nil))
	if r.Status != 200 || r.errCode(t) != -32601 {
		t.Errorf("legacy unknown method: %d %s", r.Status, r.Raw)
	}
	// The error response echoes the request id.
	if r.Body["id"] != float64(1) {
		t.Errorf("id = %v", r.Body["id"])
	}
	// Modern results carry resultType and serverInfo on every method.
	for _, m := range []string{"ping", "tools/list", "resources/templates/list", "skills/list", "resources/list"} {
		res := call(t, g, gatewaytest.ReaderToken, m, nil).result(t)
		if res["resultType"] != "complete" {
			t.Errorf("%s: resultType = %v", m, res["resultType"])
		}
		if meta, _ := res["_meta"].(map[string]any); meta["io.modelcontextprotocol/serverInfo"] == nil {
			t.Errorf("%s: no serverInfo", m)
		}
	}
	// Mcp-Name, when sent, must match params.uri / params.name.
	r = post(t, g, gatewaytest.ReaderToken, map[string]string{"MCP-Protocol-Version": modernVersion, "Mcp-Method": "resources/read", "Mcp-Name": "skill://platform/git-workflow/SKILL.md"},
		modern(1, "resources/read", map[string]any{"uri": "skill://platform/pdf-processing/SKILL.md"}))
	if r.Status != http.StatusBadRequest || r.errCode(t) != -32020 {
		t.Errorf("Mcp-Name mismatch: %d %s", r.Status, r.Raw)
	}
	b64 := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("skill://platform/pdf-processing/SKILL.md")) + "?="
	r = post(t, g, gatewaytest.ReaderToken, map[string]string{"MCP-Protocol-Version": modernVersion, "Mcp-Method": "resources/read", "Mcp-Name": b64},
		modern(1, "resources/read", map[string]any{"uri": "skill://platform/pdf-processing/SKILL.md"}))
	if r.Status != 200 {
		t.Errorf("Mcp-Name base64: %d %s", r.Status, r.Raw)
	}
}

// readText reads a skill file over resources/read and returns its bytes.
func readBytes(t *testing.T, g *gatewaytest.Gateway, token, uri string) ([]byte, string) {
	t.Helper()
	r := call(t, g, token, "resources/read", map[string]any{"uri": uri})
	if r.Status != 200 {
		t.Fatalf("resources/read %s: %d %s", uri, r.Status, r.Raw)
	}
	contents, _ := r.result(t)["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents = %v", r.result(t)["contents"])
	}
	c := contents[0].(map[string]any)
	if c["uri"] != uri {
		t.Errorf("content uri = %v, want %s", c["uri"], uri)
	}
	mt, _ := c["mimeType"].(string)
	if text, ok := c["text"].(string); ok {
		if _, hasBlob := c["blob"]; hasBlob {
			t.Errorf("%s has both text and blob", uri)
		}
		return []byte(text), mt
	}
	blob, ok := c["blob"].(string)
	if !ok {
		t.Fatalf("%s has neither text nor blob: %v", uri, c)
	}
	b, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		t.Fatalf("%s blob is not base64: %v", uri, err)
	}
	return b, mt
}

func TestSkillsList(t *testing.T) {
	g := fixture(t)
	r := call(t, g, gatewaytest.ReaderToken, "skills/list", nil)
	if r.Status != 200 {
		t.Fatalf("skills/list: %d %s", r.Status, r.Raw)
	}
	res := r.result(t)
	if res["ttlMs"] == nil || res["cacheScope"] != "private" || res["resultType"] != "complete" {
		t.Errorf("cache attributes: %v %v %v", res["ttlMs"], res["cacheScope"], res["resultType"])
	}
	if _, ok := res["nextCursor"]; ok {
		t.Errorf("unexpected nextCursor for a small catalog")
	}
	skills, _ := res["skills"].([]any)
	var uris []string
	for _, s := range skills {
		uris = append(uris, s.(map[string]any)["uri"].(string))
	}
	if strings.Join(uris, " ") != "skill://platform/git-workflow/SKILL.md skill://platform/pdf-processing/SKILL.md skill://platform/secret-ops/SKILL.md" {
		t.Fatalf("uris = %v", uris)
	}
	for _, s := range skills {
		entry := s.(map[string]any)
		uri := entry["uri"].(string)
		fm, _ := entry["frontmatter"].(map[string]any)
		if !strings.HasSuffix(uri, "/SKILL.md") {
			t.Errorf("%s: uri is not the SKILL.md", uri)
		}
		segs := strings.Split(strings.TrimSuffix(strings.TrimPrefix(uri, "skill://"), "/SKILL.md"), "/")
		if segs[len(segs)-1] != fm["name"] {
			t.Errorf("%s: final segment %q != frontmatter.name %v", uri, segs[len(segs)-1], fm["name"])
		}
		if d, _ := fm["description"].(string); d == "" {
			t.Errorf("%s: frontmatter.description missing", uri)
		}
		resources, ok := entry["resources"].([]any)
		if !ok {
			t.Fatalf("%s: resources = %v", uri, entry["resources"])
		}
		seen := map[string]bool{}
		for _, x := range resources {
			res := x.(map[string]any)
			ruri := res["uri"].(string)
			if seen[ruri] {
				t.Errorf("%s: %s listed twice", uri, ruri)
			}
			seen[ruri] = true
			if !strings.HasPrefix(ruri, strings.TrimSuffix(uri, "SKILL.md")) {
				t.Errorf("%s: resource %s is outside the skill", uri, ruri)
			}
			digest, _ := res["digest"].(string)
			if !digestRe.MatchString(digest) {
				t.Errorf("%s: digest %q malformed", ruri, digest)
			}
			size, ok := res["size"].(float64)
			if !ok {
				t.Errorf("%s: size missing", ruri)
			}
			data, _ := readBytes(t, g, gatewaytest.ReaderToken, ruri)
			sum := sha256.Sum256(data)
			if digest != "sha256:"+hex.EncodeToString(sum[:]) {
				t.Errorf("%s: digest %s != sha256 of served bytes", ruri, digest)
			}
			if int(size) != len(data) {
				t.Errorf("%s: size %v != %d served bytes", ruri, size, len(data))
			}
		}
		if !seen[uri] {
			t.Errorf("%s: resources lack the SKILL.md entry itself", uri)
		}
	}
	// The pdf-processing entry in detail: verbatim frontmatter and the
	// SEP-2640 test vector for templates/invoice.md.
	pdf := skills[1].(map[string]any)
	fm := pdf["frontmatter"].(map[string]any)
	if fm["name"] != "pdf-processing" || fm["license"] != "Apache-2.0" || fm["description"] != "Extract, fill, and assemble PDF documents" {
		t.Errorf("frontmatter = %v", fm)
	}
	if md, _ := fm["metadata"].(map[string]any); md["owner"] != "docs" {
		t.Errorf("frontmatter.metadata = %v", fm["metadata"])
	}
	if ag, _ := fm["agents"].(map[string]any); ag["cursor"].(map[string]any)["globs"] != "**/*.pdf" {
		t.Errorf("frontmatter.agents not verbatim: %v", fm["agents"])
	}
	resources := pdf["resources"].([]any)
	if len(resources) != 7 {
		t.Errorf("pdf-processing has %d resources, want 7", len(resources))
	}
	var found bool
	for _, x := range resources {
		res := x.(map[string]any)
		if res["uri"] == "skill://platform/pdf-processing/templates/invoice.md" {
			found = true
			if res["digest"] != invoiceDigest || res["size"] != float64(29) {
				t.Errorf("invoice.md = %v, want %s / 29", res, invoiceDigest)
			}
		}
	}
	if !found {
		t.Error("templates/invoice.md not listed")
	}
	// Legacy callers get the same entries without the 2026 cache fields.
	r = post(t, g, gatewaytest.ReaderToken, nil, legacy(1, "skills/list", map[string]any{}))
	if r.Status != 200 || len(r.result(t)["skills"].([]any)) != 3 {
		t.Errorf("legacy skills/list: %d %s", r.Status, r.Raw)
	}
	// An empty cursor is valid; an unknown cursor yields an empty page.
	r = call(t, g, gatewaytest.ReaderToken, "skills/list", map[string]any{"cursor": ""})
	if len(r.result(t)["skills"].([]any)) != 3 {
		t.Errorf("empty cursor: %s", r.Raw)
	}
	r = call(t, g, gatewaytest.ReaderToken, "skills/list", map[string]any{"cursor": "zzz/zzz"})
	if s, _ := r.result(t)["skills"].([]any); len(s) != 0 {
		t.Errorf("past-the-end cursor: %s", r.Raw)
	}
}

func TestSkillsGet(t *testing.T) {
	g := fixture(t)
	r := call(t, g, gatewaytest.ReaderToken, "skills/get", map[string]any{"uri": "skill://platform/pdf-processing/SKILL.md"})
	if r.Status != 200 {
		t.Fatalf("skills/get: %d %s", r.Status, r.Raw)
	}
	res := r.result(t)
	skill, _ := res["skill"].(map[string]any)
	if skill["uri"] != "skill://platform/pdf-processing/SKILL.md" {
		t.Errorf("skill.uri = %v", skill["uri"])
	}
	if fm := skill["frontmatter"].(map[string]any); fm["name"] != "pdf-processing" || fm["license"] != "Apache-2.0" {
		t.Errorf("frontmatter = %v", fm)
	}
	if rs, _ := skill["resources"].([]any); len(rs) != 7 {
		t.Errorf("resources = %v", skill["resources"])
	}
	if res["ttlMs"] == nil || res["cacheScope"] == nil || res["resultType"] != "complete" {
		t.Errorf("cache attrs missing: %v", res)
	}
	if _, ok := res["nextCursor"]; ok {
		t.Error("skills/get must not carry a cursor")
	}

	unknown := func(label, uri string) {
		t.Helper()
		r := call(t, g, gatewaytest.ReaderToken, "skills/get", map[string]any{"uri": uri})
		if r.Status != 200 || r.errCode(t) != -32602 {
			t.Errorf("%s (%s): %d %s", label, uri, r.Status, r.Raw)
			return
		}
		if !strings.Contains(r.errMessage(), uri) {
			t.Errorf("%s: message %q does not name the uri", label, r.errMessage())
		}
	}
	unknown("missing skill", "skill://platform/chargebacks/SKILL.md")
	unknown("missing namespace", "skill://nowhere/pdf-processing/SKILL.md")
	unknown("skill root, not SKILL.md", "skill://platform/pdf-processing")
	unknown("supporting file", "skill://platform/pdf-processing/templates/invoice.md")
	unknown("wrong scheme", "file:///platform/pdf-processing/SKILL.md")
	unknown("traversal", "skill://platform/pdf-processing/../refunds/SKILL.md")
	unknown("empty", "")
	unknown("uppercase", "skill://platform/PDF-Processing/SKILL.md")
	// A denied skill is indistinguishable from a missing one.
	unknown("denied (payments)", "skill://payments/refunds/SKILL.md")
	r1 := call(t, g, gatewaytest.ReaderToken, "skills/get", map[string]any{"uri": "skill://payments/refunds/SKILL.md"})
	r2 := call(t, g, gatewaytest.ReaderToken, "skills/get", map[string]any{"uri": "skill://payments/ghost/SKILL.md"})
	if strings.Replace(r1.errMessage(), "refunds", "ghost", 1) != r2.errMessage() {
		t.Errorf("denied vs missing messages differ: %q vs %q", r1.errMessage(), r2.errMessage())
	}
	// The owner can get it, and the read is audited under their subject.
	r = call(t, g, gatewaytest.PaymentsToken, "skills/get", map[string]any{"uri": "skill://payments/refunds/SKILL.md"})
	if r.Status != 200 || r.result(t)["skill"] == nil {
		t.Errorf("carol skills/get: %d %s", r.Status, r.Raw)
	}
	var buf bytes.Buffer
	if err := g.Client(gatewaytest.AdminToken).Audit(context.Background(), "", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"subject":"carol"`) || !strings.Contains(buf.String(), `"detail":"mcp:skills/get"`) {
		t.Errorf("MCP read not audited:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"subject":"dave","action":"fetch","namespace":"payments","name":"refunds","version":"1.0.0","outcome":"denied"`) {
		t.Errorf("denied MCP read not audited:\n%s", buf.String())
	}
	// A skill whose versions are all deprecated is no longer served.
	g.Client(gatewaytest.PlatformToken).Deprecate(context.Background(), "platform", "git-workflow", "1.0.0")
	unknown("deprecated", "skill://platform/git-workflow/SKILL.md")
}

func TestResourcesRead(t *testing.T) {
	g := fixture(t)
	data, mt := readBytes(t, g, gatewaytest.ReaderToken, "skill://platform/pdf-processing/SKILL.md")
	if string(data) != pdfMD {
		t.Errorf("SKILL.md text not byte-exact:\n%q\nwant\n%q", data, pdfMD)
	}
	if mt != "text/markdown" {
		t.Errorf("SKILL.md mimeType = %q", mt)
	}
	data, mt = readBytes(t, g, gatewaytest.ReaderToken, "skill://platform/pdf-processing/templates/invoice.md")
	if string(data) != invoice || mt != "text/markdown" {
		t.Errorf("invoice.md = %q %q", data, mt)
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != invoiceDigest || len(data) != 29 {
		t.Errorf("invoice.md served bytes do not match the SEP vector")
	}
	data, mt = readBytes(t, g, gatewaytest.ReaderToken, "skill://platform/pdf-processing/assets/logo.png")
	if string(data) != "\x89PNG\r\n\x1a\n\x00\x00binary" || mt != "image/png" {
		t.Errorf("logo.png = %q %q", data, mt)
	}
	r := call(t, g, gatewaytest.ReaderToken, "resources/read", map[string]any{"uri": "skill://platform/pdf-processing/assets/logo.png"})
	if c := r.result(t)["contents"].([]any)[0].(map[string]any); c["blob"] == nil || c["text"] != nil {
		t.Errorf("binary must be served as blob: %v", c)
	}
	if res := r.result(t); res["ttlMs"] == nil || res["cacheScope"] == nil {
		t.Errorf("resources/read lacks cache attributes: %v", res)
	}
	_, mt = readBytes(t, g, gatewaytest.ReaderToken, "skill://platform/pdf-processing/scripts/extract.py")
	if mt == "" || mt == "application/octet-stream" {
		t.Errorf("extract.py mimeType = %q", mt)
	}

	// Unknown, directory and denied URIs are -32602 on the modern era and
	// -32002 for legacy callers; contents is never empty.
	for label, uri := range map[string]string{
		"missing file":  "skill://platform/pdf-processing/templates/nope.md",
		"directory":     "skill://platform/pdf-processing/templates",
		"skill root":    "skill://platform/pdf-processing",
		"namespace":     "skill://platform",
		"missing skill": "skill://platform/ghost/SKILL.md",
		"denied skill":  "skill://payments/refunds/SKILL.md",
		"denied file":   "skill://payments/refunds/examples/email.md",
		"traversal":     "skill://platform/pdf-processing/../../x",
		"other scheme":  "file:///etc/passwd",
		"empty":         "",
	} {
		r := call(t, g, gatewaytest.ReaderToken, "resources/read", map[string]any{"uri": uri})
		if r.Status != 200 || r.errCode(t) != -32602 {
			t.Errorf("modern %s: %d %s", label, r.Status, r.Raw)
		}
		r = post(t, g, gatewaytest.ReaderToken, nil, legacy(1, "resources/read", map[string]any{"uri": uri}))
		if r.Status != 200 || r.errCode(t) != -32002 {
			t.Errorf("legacy %s: %d %s", label, r.Status, r.Raw)
		}
	}
	// The owner reads the denied file fine.
	if data, _ := readBytes(t, g, gatewaytest.PaymentsToken, "skill://payments/refunds/examples/email.md"); string(data) != "Dear customer,\n" {
		t.Errorf("carol email.md = %q", data)
	}
}

func TestResourcesList(t *testing.T) {
	g := fixture(t)
	r := call(t, g, gatewaytest.ReaderToken, "resources/list", nil)
	res := r.result(t)
	list, _ := res["resources"].([]any)
	byURI := map[string]map[string]any{}
	for _, x := range list {
		m := x.(map[string]any)
		byURI[m["uri"].(string)] = m
	}
	md := byURI["skill://platform/pdf-processing/SKILL.md"]
	if md == nil || md["name"] != "pdf-processing" || md["description"] != "Extract, fill, and assemble PDF documents" || md["mimeType"] != "text/markdown" || md["size"] != float64(len(pdfMD)) {
		t.Errorf("SKILL.md resource = %v", md)
	}
	if inv := byURI["skill://platform/pdf-processing/templates/invoice.md"]; inv == nil || inv["name"] != "invoice.md" || inv["mimeType"] != "text/markdown" {
		t.Errorf("invoice resource = %v", inv)
	}
	if png := byURI["skill://platform/pdf-processing/assets/logo.png"]; png == nil || png["mimeType"] != "image/png" {
		t.Errorf("png resource = %v", png)
	}
	for uri := range byURI {
		if strings.HasPrefix(uri, "skill://payments/") {
			t.Errorf("denied resource listed: %s", uri)
		}
	}
	if len(list) != 7+1+1 {
		t.Errorf("%d resources listed, want 9", len(list))
	}
	if res["ttlMs"] == nil || res["cacheScope"] == nil {
		t.Errorf("resources/list lacks cache attributes")
	}
}

func TestDirectoryRead(t *testing.T) {
	g := fixture(t)
	read := func(uri string) rpcResp {
		t.Helper()
		return call(t, g, gatewaytest.ReaderToken, "resources/directory/read", map[string]any{"uri": uri})
	}
	names := func(r rpcResp) string {
		t.Helper()
		var out []string
		for _, x := range r.result(t)["resources"].([]any) {
			m := x.(map[string]any)
			out = append(out, m["name"].(string)+":"+m["mimeType"].(string))
		}
		return strings.Join(out, " ")
	}
	r := read("skill://platform/pdf-processing")
	if r.Status != 200 {
		t.Fatalf("root: %d %s", r.Status, r.Raw)
	}
	// SKILL.md carries its ordinary Resource metadata: name is the skill name.
	if got := names(r); got != "pdf-processing:text/markdown assets:inode/directory scripts:inode/directory templates:inode/directory" {
		t.Errorf("root children = %q", got)
	}
	if r.result(t)["resultType"] != "complete" {
		t.Errorf("resultType = %v", r.result(t)["resultType"])
	}
	for _, x := range r.result(t)["resources"].([]any) {
		m := x.(map[string]any)
		uri := m["uri"].(string)
		if strings.HasSuffix(uri, "/") || !strings.HasPrefix(uri, "skill://platform/pdf-processing/") {
			t.Errorf("child uri %q", uri)
		}
		if strings.HasSuffix(uri, "/SKILL.md") && (m["description"] != "Extract, fill, and assemble PDF documents" || m["size"] != float64(len(pdfMD))) {
			t.Errorf("SKILL.md child = %v", m)
		}
	}
	r = read("skill://platform/pdf-processing/templates")
	if got := names(r); got != "invoice.md:text/markdown purchase-order.md:text/markdown regional:inode/directory" {
		t.Errorf("templates children = %q", got)
	}
	r = read("skill://platform/pdf-processing/templates/regional")
	if got := names(r); got != "eu.md:text/markdown" {
		t.Errorf("regional children = %q", got)
	}
	// Namespace level lists skills as directories; hidden ones are absent.
	r = read("skill://platform")
	if got := names(r); got != "git-workflow:inode/directory pdf-processing:inode/directory secret-ops:inode/directory" {
		t.Errorf("namespace children = %q", got)
	}
	// Files, missing directories, denied and malformed URIs: -32602.
	for label, uri := range map[string]string{
		"file":              "skill://platform/pdf-processing/SKILL.md",
		"nested file":       "skill://platform/pdf-processing/templates/invoice.md",
		"missing dir":       "skill://platform/pdf-processing/nope",
		"missing skill":     "skill://platform/ghost",
		"denied skill":      "skill://payments/refunds",
		"denied namespace":  "skill://payments",
		"unknown namespace": "skill://nowhere",
		"trailing slash":    "skill://platform/pdf-processing/templates/",
		"traversal":         "skill://platform/pdf-processing/..",
		"other scheme":      "file:///tmp",
	} {
		r := read(uri)
		if r.Status != 200 || r.errCode(t) != -32602 {
			t.Errorf("%s (%s): %d %s", label, uri, r.Status, r.Raw)
		} else if !strings.Contains(r.errMessage(), "not a directory") {
			t.Errorf("%s: message %q", label, r.errMessage())
		}
	}
	// The owner can browse the denied namespace.
	r = call(t, g, gatewaytest.PaymentsToken, "resources/directory/read", map[string]any{"uri": "skill://payments/refunds"})
	if got := names(r); got != "refunds:text/markdown examples:inode/directory" {
		t.Errorf("carol refunds children = %q", got)
	}
}

func TestTools(t *testing.T) {
	g := fixture(t)
	r := call(t, g, gatewaytest.ReaderToken, "tools/list", nil)
	tools, _ := r.result(t)["tools"].([]any)
	var names []string
	for _, x := range tools {
		tool := x.(map[string]any)
		names = append(names, tool["name"].(string))
		if tool["description"] == "" || tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v malformed", tool)
		}
		if ann, _ := tool["annotations"].(map[string]any); ann["readOnlyHint"] != true {
			t.Errorf("tool %s is not marked read-only", tool["name"])
		}
	}
	if strings.Join(names, " ") != "list_skills get_skill read_skill_file" {
		t.Errorf("tools = %v", names)
	}
	toolCall := func(token, name string, args map[string]any) (string, bool) {
		t.Helper()
		r := call(t, g, token, "tools/call", map[string]any{"name": name, "arguments": args})
		if r.Status != 200 {
			t.Fatalf("tools/call %s: %d %s", name, r.Status, r.Raw)
		}
		res := r.result(t)
		content := res["content"].([]any)[0].(map[string]any)
		isErr, _ := res["isError"].(bool)
		return content["text"].(string), isErr
	}
	text, isErr := toolCall(gatewaytest.ReaderToken, "list_skills", nil)
	if isErr || !strings.Contains(text, "- platform/pdf-processing@1.0.0 — Extract, fill, and assemble PDF documents") || strings.Contains(text, "payments/") {
		t.Errorf("list_skills = %q", text)
	}
	text, _ = toolCall(gatewaytest.ReaderToken, "list_skills", map[string]any{"query": "git"})
	if strings.Contains(text, "pdf-processing") || !strings.Contains(text, "git-workflow") {
		t.Errorf("list_skills query = %q", text)
	}
	text, _ = toolCall(gatewaytest.ReaderToken, "list_skills", map[string]any{"namespace": "payments"})
	if !strings.Contains(text, "No skills") {
		t.Errorf("list_skills denied namespace = %q", text)
	}
	text, isErr = toolCall(gatewaytest.ReaderToken, "get_skill", map[string]any{"skill": "platform/pdf-processing"})
	if isErr || !strings.Contains(text, "Skill platform/pdf-processing@1.0.0 (digest sha256:") || !strings.Contains(text, pdfMD) ||
		!strings.Contains(text, "- skill://platform/pdf-processing/templates/invoice.md (29 bytes)") {
		t.Errorf("get_skill = %q", text)
	}
	for label, args := range map[string]map[string]any{
		"denied":   {"skill": "payments/refunds"},
		"missing":  {"skill": "platform/ghost"},
		"no slash": {"skill": "pdf-processing"},
		"empty":    {},
	} {
		if text, isErr := toolCall(gatewaytest.ReaderToken, "get_skill", args); !isErr || strings.Contains(text, "Dear customer") {
			t.Errorf("get_skill %s: isError=%v %q", label, isErr, text)
		}
	}
	text, isErr = toolCall(gatewaytest.ReaderToken, "read_skill_file", map[string]any{"uri": "skill://platform/pdf-processing/templates/invoice.md"})
	if isErr || text != invoice {
		t.Errorf("read_skill_file = %q", text)
	}
	if _, isErr := toolCall(gatewaytest.ReaderToken, "read_skill_file", map[string]any{"uri": "skill://platform/pdf-processing/assets/logo.png"}); !isErr {
		t.Error("binary file should be reported as an error")
	}
	if _, isErr := toolCall(gatewaytest.ReaderToken, "read_skill_file", map[string]any{"uri": "skill://payments/refunds/examples/email.md"}); !isErr {
		t.Error("denied file readable via tool")
	}
	r = call(t, g, gatewaytest.ReaderToken, "tools/call", map[string]any{"name": "rm_rf", "arguments": map[string]any{}})
	if r.errCode(t) != -32602 {
		t.Errorf("unknown tool: %s", r.Raw)
	}
	r = post(t, g, gatewaytest.ReaderToken, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_skill","arguments":"nope"}}`)
	if r.errCode(t) != -32602 {
		t.Errorf("bad arguments: %s", r.Raw)
	}
	// Mcp-Name on tools/call must match params.name.
	r = post(t, g, gatewaytest.ReaderToken, map[string]string{"MCP-Protocol-Version": modernVersion, "Mcp-Method": "tools/call", "Mcp-Name": "list_skills"},
		modern(1, "tools/call", map[string]any{"name": "get_skill", "arguments": map[string]any{"skill": "platform/pdf-processing"}}))
	if r.Status != 400 || r.errCode(t) != -32020 {
		t.Errorf("Mcp-Name mismatch on tools/call: %d %s", r.Status, r.Raw)
	}
}

func TestPolicyFilteringAppliesToMCP(t *testing.T) {
	g := fixture(t)
	listURIs := func(token string) string {
		t.Helper()
		var out []string
		for _, s := range call(t, g, token, "skills/list", nil).result(t)["skills"].([]any) {
			out = append(out, s.(map[string]any)["uri"].(string))
		}
		return strings.Join(out, " ")
	}
	if got := listURIs(gatewaytest.BotToken); got != "skill://platform/git-workflow/SKILL.md skill://platform/pdf-processing/SKILL.md" {
		t.Errorf("robot sees %q", got)
	}
	if got := listURIs(gatewaytest.PaymentsToken); got != "skill://payments/refunds/SKILL.md skill://platform/git-workflow/SKILL.md skill://platform/pdf-processing/SKILL.md skill://platform/secret-ops/SKILL.md" {
		t.Errorf("carol sees %q", got)
	}
	r := call(t, g, gatewaytest.BotToken, "skills/get", map[string]any{"uri": "skill://platform/secret-ops/SKILL.md"})
	if r.errCode(t) != -32602 {
		t.Errorf("robot skills/get secret-ops: %s", r.Raw)
	}
	r = call(t, g, gatewaytest.BotToken, "resources/read", map[string]any{"uri": "skill://platform/secret-ops/SKILL.md"})
	if r.errCode(t) != -32602 {
		t.Errorf("robot resources/read secret-ops: %s", r.Raw)
	}
	r = call(t, g, gatewaytest.BotToken, "resources/directory/read", map[string]any{"uri": "skill://platform/secret-ops"})
	if r.errCode(t) != -32602 {
		t.Errorf("robot directory read secret-ops: %s", r.Raw)
	}
	r = call(t, g, gatewaytest.BotToken, "resources/directory/read", map[string]any{"uri": "skill://platform"})
	if s := string(r.Raw); strings.Contains(s, "secret-ops") {
		t.Errorf("robot namespace listing leaks secret-ops: %s", s)
	}
	r = call(t, g, gatewaytest.BotToken, "resources/list", nil)
	if s := string(r.Raw); strings.Contains(s, "secret-ops") {
		t.Errorf("robot resources/list leaks secret-ops: %s", s)
	}
	r = call(t, g, gatewaytest.BotToken, "tools/call", map[string]any{"name": "list_skills", "arguments": map[string]any{}})
	if s := string(r.Raw); strings.Contains(s, "secret-ops") {
		t.Errorf("robot list_skills leaks secret-ops: %s", s)
	}
	// Policy is evaluated per request on the token, never on a session.
	if got := listURIs(gatewaytest.ReaderToken); !strings.Contains(got, "secret-ops") {
		t.Errorf("dave should see secret-ops: %q", got)
	}
}
