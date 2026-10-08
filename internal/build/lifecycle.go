package build

import (
	"context"
	"os"
)

// AcquireRunner reserves the local identity until staging, collection and cleanup
// finish. Remote runners reserve their identity on the Agent instead.
func AcquireRunner(ctx context.Context, runner Runner) (func(), error) {
	if gated, ok := runner.(interface {
		AcquireBuild(context.Context) (func(), error)
	}); ok {
		return gated.AcquireBuild(ctx)
	}
	return func() {}, ctx.Err()
}

func CleanupRunnerWorkspace(runner Runner, workspace string) error {
	if cleaner, ok := runner.(interface{ CleanupWorkspace(string) error }); ok {
		return cleaner.CleanupWorkspace(workspace)
	}
	return os.RemoveAll(workspace)
}
