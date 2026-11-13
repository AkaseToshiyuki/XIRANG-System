package external

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Initialize imports a verified offline control image into an empty namespace.
// It acquires the normal exclusive leadership lock and never overwrites a row.
func Initialize(ctx context.Context, dsn, namespace string, image []byte) error {
	if len(image) == 0 {
		return errors.New("empty bootstrap image")
	}
	directory, err := os.MkdirTemp("", "xirang-postgresql-import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	state, err := Open(ctx, Config{DSN: dsn, Namespace: namespace, CachePath: filepath.Join(directory, "state.db"), bootstrapImage: image}, nil)
	if err != nil {
		return err
	}
	return state.Close()
}
