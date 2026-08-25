package notification

import (
	"errors"
	"os"
	"strings"
)

func LoadSecretFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(ErrInvalidConfig, errors.New("webhook secret path must be a regular file"))
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.Join(ErrInvalidConfig, errors.New("webhook secret file must not be accessible by group or other users"))
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(string(value))), nil
}
