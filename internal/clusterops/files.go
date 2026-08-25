package clusterops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xirang-grid/xirang/internal/domain"
)

type StreamRunner interface {
	RunStream(context.Context, domain.Node, io.Reader, io.Writer, ...string) error
}

func (service *Service) ListFiles(ctx context.Context, spaceID, path string, principal domain.Principal) ([]domain.SharedFile, error) {
	service.mu.Lock()
	defer service.mu.Unlock()

	space, source, relative, err := service.fileContextLocked(ctx, spaceID, path)
	if err != nil {
		return nil, err
	}
	if principal.Role != "admin" && relative != "" && !service.canManageFile(space, relative, principal) {
		return nil, ErrForbidden
	}
	output, err := service.runner.Run(ctx, source, "file-list", space.ID, space.SourcePath, encodeRelativePath(relative))
	if err != nil {
		return nil, err
	}
	var files []domain.SharedFile
	if err := json.Unmarshal(output, &files); err != nil {
		return nil, fmt.Errorf("decode shared-space file list: %w", err)
	}
	identity := principalIdentity(principal)
	admin := principal.Role == "admin"
	filtered := make([]domain.SharedFile, 0, len(files))
	for _, file := range files {
		owner, tracked := space.FileOwners[file.Path]
		file.OwnedByUser = tracked && owner.ID == identity.ID
		if tracked {
			file.Owner = owner.Name
		}
		if admin || file.OwnedByUser {
			filtered = append(filtered, file)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Directory != filtered[j].Directory {
			return filtered[i].Directory
		}
		return strings.ToLower(filtered[i].Name) < strings.ToLower(filtered[j].Name)
	})
	return filtered, nil
}

func (service *Service) DownloadFile(ctx context.Context, spaceID, path string, principal domain.Principal, output io.Writer) error {
	service.mu.Lock()
	defer service.mu.Unlock()

	space, source, relative, err := service.fileContextLocked(ctx, spaceID, path)
	if err != nil {
		return err
	}
	if relative == "" || !service.canManageFile(space, relative, principal) {
		return ErrForbidden
	}
	runner, ok := service.runner.(StreamRunner)
	if !ok {
		return errorsUnavailable("streaming shared files")
	}
	return runner.RunStream(ctx, source, nil, output, "file-read", space.ID, space.SourcePath, encodeRelativePath(relative))
}

func (service *Service) UploadFile(ctx context.Context, spaceID, path string, principal domain.Principal, input io.Reader) error {
	service.mu.Lock()
	defer service.mu.Unlock()

	space, source, relative, err := service.fileContextLocked(ctx, spaceID, path)
	if err != nil {
		return err
	}
	if relative == "" || !service.canCreateOrManageFile(space, relative, principal) {
		return ErrForbidden
	}
	mode := "upsert"
	if principal.Role != "admin" {
		if _, exists := space.FileOwners[relative]; exists {
			mode = "replace"
		} else {
			mode = "create"
		}
	}
	runner, ok := service.runner.(StreamRunner)
	if !ok {
		return errorsUnavailable("streaming shared files")
	}
	maximumBytes, err := service.maximumUploadBytesLocked(ctx, space, relative, principal)
	if err != nil {
		return err
	}
	counted := &countingReader{reader: input}
	if err := runner.RunStream(ctx, source, counted, io.Discard, "file-write", space.ID, space.SourcePath, encodeRelativePath(relative), mode, fmt.Sprintf("%d", maximumBytes)); err != nil {
		return err
	}
	service.invalidateCapacity(space.ID)
	if space.FileOwners == nil {
		space.FileOwners = map[string]domain.SharedFileOwner{}
	}
	owner := principalIdentity(principal)
	owner.SizeBytes = counted.bytes
	space.FileOwners[relative] = owner
	space.UpdatedAt = service.now().UTC()
	return service.repository.PutSharedSpace(ctx, space)
}

func (service *Service) CreateDirectory(ctx context.Context, spaceID, path string, principal domain.Principal) error {
	service.mu.Lock()
	defer service.mu.Unlock()

	space, source, relative, err := service.fileContextLocked(ctx, spaceID, path)
	if err != nil {
		return err
	}
	if relative == "" || !service.canCreateOrManageFile(space, relative, principal) {
		return ErrForbidden
	}
	// Folder uploads may revisit a directory that this user already created.
	// Treat the tracked directory as idempotent without claiming an untracked
	// filesystem entry that happens to have the same name.
	if _, exists := space.FileOwners[relative]; exists {
		return nil
	}
	if _, err := service.runner.Run(ctx, source, "file-mkdir", space.ID, space.SourcePath, encodeRelativePath(relative)); err != nil {
		return err
	}
	service.invalidateCapacity(space.ID)
	if space.FileOwners == nil {
		space.FileOwners = map[string]domain.SharedFileOwner{}
	}
	owner := principalIdentity(principal)
	owner.Directory = true
	space.FileOwners[relative] = owner
	space.UpdatedAt = service.now().UTC()
	return service.repository.PutSharedSpace(ctx, space)
}

func (service *Service) RenameFile(ctx context.Context, spaceID, oldPath, newPath string, principal domain.Principal) error {
	service.mu.Lock()
	defer service.mu.Unlock()

	space, source, oldRelative, err := service.fileContextLocked(ctx, spaceID, oldPath)
	if err != nil {
		return err
	}
	newRelative, err := cleanRelativePath(newPath)
	if err != nil || oldRelative == "" || newRelative == "" {
		return ErrInvalid
	}
	if !service.canManageFile(space, oldRelative, principal) || !service.canCreateOrManageFile(space, newRelative, principal) {
		return ErrForbidden
	}
	if principal.Role != "admin" {
		if _, err := ownedSubtreeManifest(space, oldRelative, principalIdentity(principal).ID); err != nil {
			return err
		}
	}
	if _, err := service.runner.Run(ctx, source, "file-rename", space.ID, space.SourcePath, encodeRelativePath(oldRelative), encodeRelativePath(newRelative)); err != nil {
		return err
	}
	service.invalidateCapacity(space.ID)
	if space.FileOwners == nil {
		space.FileOwners = map[string]domain.SharedFileOwner{}
	}
	moved := make(map[string]domain.SharedFileOwner)
	for path, owner := range space.FileOwners {
		if path == oldRelative || strings.HasPrefix(path, oldRelative+"/") {
			suffix := strings.TrimPrefix(path, oldRelative)
			moved[newRelative+suffix] = owner
			delete(space.FileOwners, path)
		}
	}
	for path, owner := range moved {
		space.FileOwners[path] = owner
	}
	space.UpdatedAt = service.now().UTC()
	return service.repository.PutSharedSpace(ctx, space)
}

func (service *Service) DeleteFile(ctx context.Context, spaceID, path string, principal domain.Principal) error {
	service.mu.Lock()
	defer service.mu.Unlock()

	space, source, relative, err := service.fileContextLocked(ctx, spaceID, path)
	if err != nil {
		return err
	}
	if relative == "" || !service.canManageFile(space, relative, principal) {
		return ErrForbidden
	}
	arguments := []string{"file-delete", space.ID, space.SourcePath, encodeRelativePath(relative), "recursive"}
	if principal.Role != "admin" {
		allowed, allowedErr := ownedSubtreeManifest(space, relative, principalIdentity(principal).ID)
		if allowedErr != nil {
			return allowedErr
		}
		manifest, marshalErr := json.Marshal(allowed)
		if marshalErr != nil {
			return marshalErr
		}
		arguments[4] = "owned-recursive"
		arguments = append(arguments, base64.RawURLEncoding.EncodeToString(manifest))
	}
	if _, err := service.runner.Run(ctx, source, arguments...); err != nil {
		return err
	}
	service.invalidateCapacity(space.ID)
	for path := range space.FileOwners {
		if path == relative || strings.HasPrefix(path, relative+"/") {
			delete(space.FileOwners, path)
		}
	}
	space.UpdatedAt = service.now().UTC()
	return service.repository.PutSharedSpace(ctx, space)
}

func ownedSubtreeManifest(space domain.SharedSpace, root, ownerID string) ([]string, error) {
	paths := make([]string, 0)
	for path, owner := range space.FileOwners {
		if path != root && !strings.HasPrefix(path, root+"/") {
			continue
		}
		if owner.ID != ownerID {
			return nil, ErrForbidden
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, ErrForbidden
	}
	sort.Strings(paths)
	return paths, nil
}

func (service *Service) fileContextLocked(ctx context.Context, spaceID, path string) (domain.SharedSpace, domain.Node, string, error) {
	space, err := service.repository.GetSharedSpace(ctx, spaceID)
	if err != nil {
		return domain.SharedSpace{}, domain.Node{}, "", err
	}
	source, err := service.nodes.GetNode(ctx, space.SourceNodeID)
	if err != nil {
		return domain.SharedSpace{}, domain.Node{}, "", err
	}
	relative, err := cleanRelativePath(path)
	if err != nil {
		return domain.SharedSpace{}, domain.Node{}, "", err
	}
	return space, source, relative, nil
}

func (service *Service) canManageFile(space domain.SharedSpace, path string, principal domain.Principal) bool {
	if principal.Role == "admin" {
		return true
	}
	owner, ok := space.FileOwners[path]
	return ok && owner.ID == principalIdentity(principal).ID
}

func (service *Service) canCreateOrManageFile(space domain.SharedSpace, path string, principal domain.Principal) bool {
	if principal.Role == "admin" {
		return true
	}
	if owner, exists := space.FileOwners[path]; exists {
		return owner.ID == principalIdentity(principal).ID
	}
	parent := filepath.ToSlash(filepath.Dir(path))
	if parent == "." || parent == "" {
		return true
	}
	return service.canManageFile(space, parent, principal)
}

func principalIdentity(principal domain.Principal) domain.SharedFileOwner {
	id := strings.TrimSpace(principal.UserID)
	if id == "" {
		id = strings.TrimSpace(principal.Subject)
	}
	name := strings.TrimSpace(principal.DisplayName)
	if name == "" {
		name = strings.TrimSpace(principal.Subject)
	}
	return domain.SharedFileOwner{ID: id, Name: name}
}

const maximumSharedFileBytes = uint64(64 << 30)

type countingReader struct {
	reader io.Reader
	bytes  uint64
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	reader.bytes += uint64(count)
	return count, err
}

func (service *Service) maximumUploadBytesLocked(ctx context.Context, current domain.SharedSpace, relative string, principal domain.Principal) (uint64, error) {
	if principal.Role == "admin" || service.storageQuotaFor == nil {
		return maximumSharedFileBytes, nil
	}
	quota, err := service.storageQuotaFor(ctx, principal)
	if err != nil {
		return 0, err
	}
	if quota <= 0 {
		return maximumSharedFileBytes, nil
	}
	spaces, err := service.repository.ListSharedSpaces(ctx)
	if err != nil {
		return 0, err
	}
	identity := principalIdentity(principal)
	var used uint64
	for _, space := range spaces {
		for _, owner := range space.FileOwners {
			if owner.ID == identity.ID && !owner.Directory {
				if ^uint64(0)-used < owner.SizeBytes {
					return 0, ErrInvalid
				}
				used += owner.SizeBytes
			}
		}
	}
	if old, ok := current.FileOwners[relative]; ok && old.ID == identity.ID && !old.Directory && used >= old.SizeBytes {
		used -= old.SizeBytes
	}
	limit := uint64(quota)
	if used >= limit {
		return 0, fmt.Errorf("%w: storage quota is exhausted", ErrForbidden)
	}
	remaining := limit - used
	if remaining > maximumSharedFileBytes {
		remaining = maximumSharedFileBytes
	}
	return remaining, nil
}

func cleanRelativePath(value string) (string, error) {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if value == "" || value == "." {
		return "", nil
	}
	if strings.ContainsRune(value, 0) || filepath.IsAbs(value) || len(value) > 1024 {
		return "", ErrInvalid
	}
	cleaned := filepath.ToSlash(filepath.Clean(value))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", ErrInvalid
	}
	return cleaned, nil
}

func encodeRelativePath(path string) string {
	if path == "" {
		return "_"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(path))
}

func errorsUnavailable(feature string) error {
	return fmt.Errorf("%w: %s is unavailable", ErrInvalid, feature)
}
