package server

import (
	"context"
	"net/http"

	"github.com/mthamil107/skills-gateway/internal/reqid"
)

func contextWith(r *http.Request, id string) context.Context { return reqid.With(r.Context(), id) }

// RequestID returns the request id assigned by the server, if any.
func RequestID(ctx context.Context) string { return reqid.From(ctx) }
