// Package store persists skill versions, their files and the audit log.
package store

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/mthamil107/skills-gateway/internal/bundle"
)

// Status is a version's lifecycle state. v0.1 has no draft/review flow:
// a version is published on upload and can later be deprecated.
type Status string

const (
	Published  Status = "published"
	Deprecated Status = "deprecated"
)

// Version is one immutable published version of a skill.
type Version struct {
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Status      Status            `json:"status"`
	Digest      string            `json:"digest"`
	Description string            `json:"description"`
	License     string            `json:"license,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Publisher   string            `json:"publisher"`
	PublishedAt time.Time         `json:"published_at"`
	Files       []bundle.File     `json:"files,omitempty"` // manifest; content loaded on demand
}

// Event is one audit record.
type Event struct {
	ID         int64     `json:"id"`
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id,omitempty"`
	Subject    string    `json:"subject,omitempty"`
	AgentType  string    `json:"agent_type,omitempty"`
	Action     string    `json:"action"`
	Namespace  string    `json:"namespace,omitempty"`
	Name       string    `json:"name,omitempty"`
	Version    string    `json:"version,omitempty"`
	Outcome    string    `json:"outcome"` // ok | denied | error
	Detail     string    `json:"detail,omitempty"`
	RemoteAddr string    `json:"remote_addr,omitempty"`
}

// ListFilter narrows ListLatest.
type ListFilter struct {
	Namespace string
	Query     string // substring match on name/description
	Tag       string
}

var (
	ErrNotFound      = errors.New("not found")
	ErrVersionExists = errors.New("version already exists")
)

// Store is the persistence contract. Implementations must make
// PutVersion+AppendAudit atomic when called through Tx.
type Store interface {
	// PutVersion stores a new version and its files. It returns
	// ErrVersionExists if (namespace, name, version) is already present.
	PutVersion(ctx context.Context, v Version, files []bundle.File) error
	GetVersion(ctx context.Context, ns, name, version string) (Version, error)
	ListVersions(ctx context.Context, ns, name string) ([]Version, error)
	// ListAll returns every version matching the filter, ordered by
	// namespace, name. Callers pick the latest per skill.
	ListAll(ctx context.Context, f ListFilter) ([]Version, error)
	FileContent(ctx context.Context, ns, name, version, path string) ([]byte, error)
	SetStatus(ctx context.Context, ns, name, version string, s Status) error
	AppendAudit(ctx context.Context, e Event) error
	ExportAudit(ctx context.Context, since time.Time, w io.Writer) error
	Tx(ctx context.Context, fn func(Store) error) error
	Close() error
}
