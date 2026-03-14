package toolreg

import "context"

type progressKey struct{}

// ProgressFunc is called with a status string to report progress.
type ProgressFunc func(string)

// WithProgress attaches a progress reporting function to the context.
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// ReportProgress calls the progress function attached to the context, if any.
func ReportProgress(ctx context.Context, status string) {
	if fn, ok := ctx.Value(progressKey{}).(ProgressFunc); ok && fn != nil {
		fn(status)
	}
}
