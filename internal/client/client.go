// Package client is a small Go client for the Skills Gateway REST API,
// used by the sgw CLI and the end-to-end tests.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/registry"
	"github.com/mthamil107/skills-gateway/internal/store"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

// Client talks to one gateway.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Error is a non-2xx response decoded from the error envelope.
type Error struct {
	Status    int
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d %s: %s (request %s)", e.Status, e.Code, e.Message, e.RequestID)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var env struct{ Error Error }
		json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env)
		env.Error.Status = resp.StatusCode
		return nil, &env.Error
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func skillPath(ns, name, version string) string {
	return "/v1/skills/" + url.PathEscape(ns) + "/" + url.PathEscape(name) + "/versions/" + url.PathEscape(version)
}

// Publish uploads files as a new version, sending the expected digest.
func (c *Client) Publish(ctx context.Context, ns, name, version string, files []bundle.File) (store.Version, error) {
	b := bundle.FromFiles(files)
	data, err := bundle.Encode(b.Files)
	if err != nil {
		return store.Version{}, err
	}
	resp, err := c.do(ctx, http.MethodPut, skillPath(ns, name, version), bytes.NewReader(data),
		map[string]string{"Content-Type": "application/gzip", "X-Bundle-Digest": b.Digest})
	if err != nil {
		return store.Version{}, err
	}
	defer resp.Body.Close()
	var v store.Version
	return v, json.NewDecoder(resp.Body).Decode(&v)
}

// List returns one page of skills.
func (c *Client) List(ctx context.Context, namespace, query, tag, cursor string, limit int) (registry.Page, error) {
	q := url.Values{}
	for k, v := range map[string]string{"namespace": namespace, "q": query, "tag": tag, "cursor": cursor} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var p registry.Page
	return p, c.getJSON(ctx, "/v1/skills?"+q.Encode(), &p)
}

// Get returns version metadata and manifest.
func (c *Client) Get(ctx context.Context, ns, name, version string) (store.Version, error) {
	var v store.Version
	return v, c.getJSON(ctx, skillPath(ns, name, version), &v)
}

// Bundle downloads and verifies a bundle: the archive must decode safely
// and its computed digest must equal the digest the server advertised.
func (c *Client) Bundle(ctx context.Context, ns, name, version string) (*bundle.Bundle, error) {
	resp, err := c.do(ctx, http.MethodGet, skillPath(ns, name, version)+"/bundle", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := bundle.Read(resp.Body, bundle.DefaultLimits)
	if err != nil {
		return nil, err
	}
	if want := resp.Header.Get("X-Bundle-Digest"); want != b.Digest {
		return nil, fmt.Errorf("integrity check failed: server advertised %s, received %s", want, b.Digest)
	}
	return b, nil
}

// Translation is the translate endpoint's response.
type Translation struct {
	Namespace string           `json:"namespace"`
	Name      string           `json:"name"`
	Version   string           `json:"version"`
	Digest    string           `json:"digest"`
	Result    translate.Result `json:"result"`
}

// Translate returns the files for one agent format.
func (c *Client) Translate(ctx context.Context, ns, name, version, format string) (Translation, error) {
	var t Translation
	return t, c.getJSON(ctx, skillPath(ns, name, version)+"/translate?format="+url.QueryEscape(format), &t)
}

// Deprecate marks a version deprecated.
func (c *Client) Deprecate(ctx context.Context, ns, name, version string) (store.Version, error) {
	resp, err := c.do(ctx, http.MethodPost, skillPath(ns, name, version)+"/deprecate", nil, nil)
	if err != nil {
		return store.Version{}, err
	}
	defer resp.Body.Close()
	var v store.Version
	return v, json.NewDecoder(resp.Body).Decode(&v)
}

// Audit streams the audit log as JSON Lines into w.
func (c *Client) Audit(ctx context.Context, since string, w io.Writer) error {
	p := "/v1/audit"
	if since != "" {
		p += "?since=" + url.QueryEscape(since)
	}
	resp, err := c.do(ctx, http.MethodGet, p, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}
