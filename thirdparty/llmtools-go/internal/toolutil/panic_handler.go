package toolutil

import (
	"fmt"
	"runtime/debug"

	"github.com/flexigpt/llmtools-go/internal/logutil"
)

func WithRecoveryResp[T any](fn func() (T, error)) (result T, err error) {
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("panic recovered", "panic", r, "stack", string(debug.Stack()))

			// Ensure returned values reflect failure.
			var zero T
			result = zero

			if e, ok := r.(error); ok {
				err = fmt.Errorf("panic recovered: %w", e)
			} else {
				err = fmt.Errorf("panic recovered: %v", r)
			}
		}
	}()

	result, err = fn()
	return result, err
}
