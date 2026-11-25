package llm

import "context"

type attemptRefKey struct{}

// WithAttemptReference returns a context carrying the "<provider>::<model>"
// reference of the candidate attempting the current turn call. The
// orchestrator installs it around each candidate attempt of an owner Turn or
// Subagent dispatch, so a tool that runs during the attempt (okf_write
// stamping x-ready-model) sees the model that emitted the tool call rather
// than the configured default. Probes and auxiliary consumers install none.
func WithAttemptReference(ctx context.Context, ref string) context.Context {
	return context.WithValue(ctx, attemptRefKey{}, ref)
}

// AttemptReference returns the reference installed by WithAttemptReference
// and whether one is present.
func AttemptReference(ctx context.Context) (string, bool) {
	ref, ok := ctx.Value(attemptRefKey{}).(string)

	return ref, ok && ref != ""
}
