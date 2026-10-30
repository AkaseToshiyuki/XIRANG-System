package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/clusterops"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

type matrixNodes struct{ node domain.Node }

func (nodes matrixNodes) GetNode(context.Context, string) (domain.Node, error) {
	return nodes.node, nil
}
func (nodes matrixNodes) ListNodes(context.Context) ([]domain.Node, error) {
	return []domain.Node{nodes.node}, nil
}

type matrixFiles struct{ calls int }

func (files *matrixFiles) Run(_ context.Context, _ domain.Node, args ...string) ([]byte, error) {
	files.calls++
	switch args[0] {
	case "stats":
		return []byte(`{"usedBytes":100,"availableBytes":1000000,"totalBytes":1000100}`), nil
	case "file-list":
		return json.Marshal([]domain.SharedFile{{Name: "alice.txt", Path: "alice.txt"}, {Name: "bob.txt", Path: "bob.txt"}, {Name: "untracked.txt", Path: "untracked.txt"}})
	default:
		return nil, nil
	}
}
func (files *matrixFiles) RunStream(_ context.Context, _ domain.Node, input io.Reader, output io.Writer, args ...string) error {
	files.calls++
	if args[0] == "file-read" {
		_, err := io.WriteString(output, "file contents")
		return err
	}
	if input != nil {
		_, err := io.Copy(io.Discard, input)
		return err
	}
	return nil
}

func TestSharedSpaceHTTPMultiTenantAccessMatrix(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	access := identity.NewAccessService(store, nil, nil)
	tokens := map[string]string{"admin": "admin", "anonymous": ""}
	for _, item := range []struct{ name, subject, role string }{{"alice", "alice", "user"}, {"bob", "bob", "user"}, {"viewer", "alice", "viewer"}, {"builder", "alice", "builder"}} {
		token, err := access.CreateToken(ctx, item.name, item.role, time.Hour, domain.Principal{Subject: item.subject, Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap"})
		if err != nil {
			t.Fatal(err)
		}
		tokens[item.name] = token.Secret
	}
	node := domain.Node{ID: "storage", ManagementAddress: "127.0.0.1", Roles: []domain.NodeRole{domain.NodeRoleManagement, domain.NodeRoleStorage}, Status: domain.NodeStatusReady, Capabilities: []string{mountcontrol.Capability}}
	files := &matrixFiles{}
	service := clusterops.New(store, matrixNodes{node}, files)
	handler := New(Config{AdminToken: "admin", Access: access, SharedSpaces: service}).HTTPHandler()
	seed := func() {
		t.Helper()
		err := store.PutSharedSpace(ctx, domain.SharedSpace{ID: "share", Name: "shared", SourceNodeID: node.ID, SourcePath: "/srv/share", MountPath: "/srv/share", Target: domain.SharedSpaceAll, Status: "active", FileOwners: map[string]domain.SharedFileOwner{"alice.txt": {ID: "alice", Name: "Alice"}, "bob.txt": {ID: "bob", Name: "Bob"}, "alice-dir": {ID: "alice", Directory: true}, "alice-dir/foreign": {ID: "bob"}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	path := "/api/v1/shared-spaces/share/files/content?path="
	for _, tc := range []struct {
		name, actor, method, path, body string
		want                            int
		noRunner                        bool
	}{
		{"anonymous_read", "anonymous", "GET", path + "alice.txt", "", 401, true},
		{"owner_read", "alice", "GET", path + "alice.txt", "", 200, false},
		{"foreign_read", "bob", "GET", path + "alice.txt", "", 403, true},
		{"viewer_owned_read", "viewer", "GET", path + "alice.txt", "", 200, false},
		{"viewer_write", "viewer", "PUT", path + "alice.txt", "x", 403, true},
		{"owner_write", "alice", "PUT", path + "alice.txt", "x", 204, false},
		{"foreign_write", "alice", "PUT", path + "bob.txt", "x", 403, true},
		{"builder_owner_read", "builder", "GET", path + "alice.txt", "", 200, false},
		{"builder_foreign_read", "builder", "GET", path + "bob.txt", "", 403, true},
		{"admin_foreign_write", "admin", "PUT", path + "bob.txt", "x", 204, false},
		{"untracked_read", "alice", "GET", path + "untracked.txt", "", 403, true},
		{"admin_untracked_read", "admin", "GET", path + "untracked.txt", "", 200, false},
		{"traversal_read", "alice", "GET", path + "..%2Fsecret", "", 400, true},
		{"mixed_owner_recursive_delete", "alice", "DELETE", "/api/v1/shared-spaces/share/files?path=alice-dir", "", 403, true},
		{"foreign_rename", "alice", "POST", "/api/v1/shared-spaces/share/files/rename", `{"oldPath":"bob.txt","newPath":"stolen.txt"}`, 403, true},
		{"user_export", "alice", "POST", "/api/v1/shared-spaces", `{}`, 403, true},
		{"builder_export", "builder", "POST", "/api/v1/shared-spaces", `{}`, 403, true},
		{"viewer_unshare", "viewer", "POST", "/api/v1/shared-spaces/share/unshare", `{}`, 403, true},
		{"user_unshare", "alice", "DELETE", "/api/v1/shared-spaces/share", "", 403, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed()
			before := files.calls
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if token := tokens[tc.actor]; token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("got %d want %d: %s", response.Code, tc.want, response.Body.String())
			}
			if tc.noRunner && files.calls != before {
				t.Fatal("denied request reached host runner")
			}
		})
	}
	seed()
	for _, actor := range []string{"alice", "bob", "viewer", "builder", "admin"} {
		response := jsonRequest(t, handler, http.MethodGet, "/api/v1/shared-spaces/share/files", nil, tokens[actor])
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
		var body struct {
			Items []domain.SharedFile `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		want := 1
		if actor == "admin" {
			want = 3
		}
		if len(body.Items) != want {
			t.Fatalf("%s sees %d files", actor, len(body.Items))
		}
		if actor != "admin" {
			expected := "alice.txt"
			if actor == "bob" {
				expected = "bob.txt"
			}
			if body.Items[0].Path != expected {
				t.Fatal("ownership list leaked foreign file")
			}
		}
	}
	response := jsonRequest(t, handler, http.MethodPost, "/api/v1/shared-spaces", domain.SharedSpace{Name: "Admin Share", SourceNodeID: node.ID, SourcePath: "/srv/newshare", MountPath: "/srv/newshare", Target: domain.SharedSpaceAll}, tokens["admin"])
	if response.Code != 201 {
		t.Fatalf("admin create: %d %s", response.Code, response.Body.String())
	}
}
