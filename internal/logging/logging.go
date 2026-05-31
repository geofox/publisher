// Package logging adds context-carried structured attributes to slog records.
// Stamp an attribute once with With(ctx, "post_id", id); every record emitted
// via *Context logging methods (InfoContext, ErrorContext, …) on a handler
// wrapped by ContextHandler then carries it — so a post's whole fan-out shares
// one post_id in Loki without each call site repeating it.
package logging

import (
	"context"
	"log/slog"
)

type ctxKey struct{}

// With returns a child context carrying additional structured attributes
// (key/value pairs, like slog) to be added to every subsequent log record.
func With(ctx context.Context, args ...any) context.Context {
	prev, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	var r slog.Record
	r.Add(args...)
	merged := append([]slog.Attr(nil), prev...)
	r.Attrs(func(a slog.Attr) bool { merged = append(merged, a); return true })
	return context.WithValue(ctx, ctxKey{}, merged)
}

// ContextHandler wraps a slog.Handler, injecting attributes stamped via With.
type ContextHandler struct{ slog.Handler }

func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(ctxKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}
