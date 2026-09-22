package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mthamil107/skills-gateway/internal/store"
)

// Tool shims for clients that do not implement the Skills Extension.
var tools = []map[string]any{
	{
		"name":        "list_skills",
		"description": "List the Agent Skills you are authorized to use. Optional filters: query (text), namespace.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":     map[string]any{"type": "string", "description": "Substring to match in skill name or description"},
				"namespace": map[string]any{"type": "string"},
			},
		},
		"annotations": map[string]any{"readOnlyHint": true},
	},
	{
		"name":        "get_skill",
		"description": "Load a skill's SKILL.md instructions and its file list. Pass the skill as namespace/name.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"skill": map[string]any{"type": "string", "description": "namespace/name, e.g. platform/code-review"}},
			"required":   []string{"skill"},
		},
		"annotations": map[string]any{"readOnlyHint": true},
	},
	{
		"name":        "read_skill_file",
		"description": "Read a supporting file of a skill by its skill:// URI, as listed by get_skill.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"uri": map[string]any{"type": "string"}},
			"required":   []string{"uri"},
		},
		"annotations": map[string]any{"readOnlyHint": true},
	},
}

func textResult(c *call, text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
}

func (h *Handler) toolsCall(c *call, params json.RawMessage) (map[string]any, *rpcError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errf(codeInvalidParams, "invalid tools/call params")
	}
	arg := func(k string) string { s, _ := p.Arguments[k].(string); return s }
	switch p.Name {
	case "list_skills":
		var lines []string
		cursor := ""
		for {
			page, err := h.reg.List(c.ctx, c.p, store.ListFilter{Namespace: arg("namespace"), Query: arg("query")}, 200, cursor)
			if err != nil {
				return nil, internal(err)
			}
			for _, v := range page.Items {
				lines = append(lines, fmt.Sprintf("- %s/%s@%s — %s", v.Namespace, v.Name, v.Version, v.Description))
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if len(lines) == 0 {
			return textResult(c, "No skills are available to you with those filters.", false), nil
		}
		return textResult(c, strings.Join(lines, "\n"), false), nil

	case "get_skill":
		ns, name, ok := strings.Cut(arg("skill"), "/")
		if !ok {
			return textResult(c, "skill must be namespace/name", true), nil
		}
		l, err := h.load(c, ns, name, "mcp:tool:get_skill")
		if err != nil {
			return textResult(c, "No skill "+arg("skill")+" is available to you.", true), nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Skill %s/%s@%s (digest %s)\n\n", l.v.Namespace, l.v.Name, l.v.Version, l.v.Digest)
		md, err := h.content(c, l, "SKILL.md")
		if err != nil {
			return nil, internal(err)
		}
		b.Write(md)
		if len(l.files) > 1 {
			b.WriteString("\n\n---\nSupporting files (read with read_skill_file):\n")
			for _, f := range l.files {
				if f.Path != "SKILL.md" {
					fmt.Fprintf(&b, "- %s/%s (%d bytes)\n", skillURI(l.v.Namespace, l.v.Name), f.Path, f.Size)
				}
			}
		}
		return textResult(c, b.String(), false), nil

	case "read_skill_file":
		res, rerr := h.resourcesRead(c, mustJSON(map[string]string{"uri": arg("uri")}))
		if rerr != nil {
			return textResult(c, "File not found or not available to you: "+arg("uri"), true), nil
		}
		content := res["contents"].([]any)[0].(map[string]any)
		if t, ok := content["text"].(string); ok {
			return textResult(c, t, false), nil
		}
		return textResult(c, "This file is binary; fetch it with resources/read or the REST API.", true), nil
	}
	return nil, errf(codeInvalidParams, "unknown tool %q", p.Name)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
