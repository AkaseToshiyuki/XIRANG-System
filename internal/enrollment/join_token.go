package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type JoinTokenManager struct {
	mu      sync.Mutex
	path    string
	service *Service
	current CreatedToken
}

func OpenJoinTokenManager(ctx context.Context, path string, service *Service) (*JoinTokenManager, bool, error) {
	manager := &JoinTokenManager{path: path, service: service}
	encoded, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(encoded, &manager.current); err != nil {
			return nil, false, err
		}
		return manager, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	if _, err := manager.Reset(ctx); err != nil {
		return nil, false, err
	}
	return manager, true, nil
}

func (manager *JoinTokenManager) View() CreatedToken {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.current
}

func (manager *JoinTokenManager) Reset(ctx context.Context) (CreatedToken, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current.Token.ID != "" {
		if err := manager.service.RevokeToken(ctx, manager.current.Token.ID); err != nil {
			return CreatedToken{}, err
		}
		manager.current = CreatedToken{}
	}
	created, err := manager.service.CreateToken(ctx, TokenOptions{
		Name: "cluster-join", TTL: 24 * time.Hour, MaxUses: 1, RequireApproval: false,
	})
	if err != nil {
		return CreatedToken{}, err
	}
	if err := writeSecretFile(manager.path, created); err != nil {
		_ = manager.service.RevokeToken(context.WithoutCancel(ctx), created.Token.ID)
		return CreatedToken{}, err
	}
	manager.current = created
	return created, nil
}

func writeSecretFile(path string, token CreatedToken) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(token)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}
