package mountcontrol

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
)

const Capability = "shared-mount-control-v1"
const Timeout = 30 * time.Second

// Arguments is the complete remote mutation allowlist. File I/O and local
// systemd policy administration are deliberately not operations in this protocol.
func Arguments(operation *xirangv1.SharedMountOperation, now time.Time) ([]string, error) {
	if operation == nil || !safeToken(operation.OperationId, 96) || !safeToken(operation.SpaceId, 96) ||
		operation.ExpiresAt == nil || operation.ExpiresAt.CheckValid() != nil ||
		!operation.ExpiresAt.AsTime().After(now) || operation.ExpiresAt.AsTime().After(now.Add(Timeout)) {
		return nil, errors.New("invalid or expired shared mount operation")
	}
	args := []string{"", operation.SpaceId}
	switch operation.Action {
	case xirangv1.SharedMountOperation_ACTION_PREPARE, xirangv1.SharedMountOperation_ACTION_EXPORT, xirangv1.SharedMountOperation_ACTION_UNEXPORT:
		if !safePath(operation.SourcePath) || operation.SourceHost != "" || operation.MountPath != "" {
			return nil, errors.New("invalid shared source path")
		}
		args[0] = map[xirangv1.SharedMountOperation_Action]string{xirangv1.SharedMountOperation_ACTION_PREPARE: "prepare", xirangv1.SharedMountOperation_ACTION_EXPORT: "export", xirangv1.SharedMountOperation_ACTION_UNEXPORT: "unexport"}[operation.Action]
		args = append(args, operation.SourcePath)
	case xirangv1.SharedMountOperation_ACTION_MOUNT:
		if !safeHost(operation.SourceHost) || !safePath(operation.SourcePath) || !safePath(operation.MountPath) {
			return nil, errors.New("invalid shared mount paths or host")
		}
		args[0] = "mount"
		args = append(args, operation.SourceHost, operation.SourcePath, operation.MountPath)
	case xirangv1.SharedMountOperation_ACTION_UNMOUNT:
		if !safePath(operation.MountPath) || operation.SourceHost != "" || operation.SourcePath != "" {
			return nil, errors.New("invalid shared unmount path")
		}
		args[0] = "unmount"
		args = append(args, operation.MountPath)
	default:
		return nil, errors.New("unsupported shared mount action")
	}
	return args, nil
}

func safeToken(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func safePath(value string) bool {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" || len(value) > 512 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/._-", c)) {
			return false
		}
	}
	return true
}

func safeHost(value string) bool {
	if value == "" || len(value) > 253 || strings.HasPrefix(value, "-") {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}
