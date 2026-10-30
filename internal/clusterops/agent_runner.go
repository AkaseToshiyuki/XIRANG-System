package clusterops

import (
	"context"
	"io"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
)

// AgentRunner sends every mount/export mutation over the authenticated Agent
// channel. Existing SSH file data and capacity reads retain their own boundary.
type AgentRunner struct {
	Mounts *mountcontrol.Broker
	Files  SSHRunner
}

func (runner AgentRunner) Run(ctx context.Context, node domain.Node, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, ErrInvalid
	}
	operation := &xirangv1.SharedMountOperation{}
	switch args[0] {
	case "prepare", "export", "unexport":
		if len(args) != 3 {
			return nil, ErrInvalid
		}
		operation.Action = map[string]xirangv1.SharedMountOperation_Action{"prepare": xirangv1.SharedMountOperation_ACTION_PREPARE, "export": xirangv1.SharedMountOperation_ACTION_EXPORT, "unexport": xirangv1.SharedMountOperation_ACTION_UNEXPORT}[args[0]]
		operation.SpaceId, operation.SourcePath = args[1], args[2]
	case "mount":
		if len(args) != 5 {
			return nil, ErrInvalid
		}
		operation.Action = xirangv1.SharedMountOperation_ACTION_MOUNT
		operation.SpaceId, operation.SourceHost, operation.SourcePath, operation.MountPath = args[1], args[2], args[3], args[4]
	case "unmount":
		if len(args) != 3 {
			return nil, ErrInvalid
		}
		operation.Action = xirangv1.SharedMountOperation_ACTION_UNMOUNT
		operation.SpaceId, operation.MountPath = args[1], args[2]
	case "stats", "file-list", "file-mkdir", "file-rename", "file-delete":
		return runner.Files.Run(ctx, node, args...)
	default:
		return nil, ErrInvalid
	}
	return nil, runner.Mounts.Run(ctx, node, operation)
}

func (runner AgentRunner) RunStream(ctx context.Context, node domain.Node, input io.Reader, output io.Writer, args ...string) error {
	if len(args) == 0 || (args[0] != "file-read" && args[0] != "file-write") {
		return ErrInvalid
	}
	return runner.Files.RunStream(ctx, node, input, output, args...)
}
