// Package reqid carries the per-request id through contexts.
package reqid

import "context"

type key struct{}

// With returns ctx carrying id.
func With(ctx context.Context, id string) context.Context { return context.WithValue(ctx, key{}, id) }

// From returns the request id in ctx, or "".
func From(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}
