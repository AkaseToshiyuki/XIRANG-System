package enrollment

import (
	"testing"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestParseTokenKeepsBase64URLCharacters(t *testing.T) {
	id, secret, err := parseToken("xet.id_with_underscore.secret_with_underscore-and-dash")
	if err != nil {
		t.Fatal(err)
	}
	if id != "id_with_underscore" || secret != "secret_with_underscore-and-dash" {
		t.Fatalf("unexpected parsed token: id=%q secret=%q", id, secret)
	}
}

func TestSetManagementRolePreservesOtherRoles(t *testing.T) {
	roles := setManagementRole([]domain.NodeRole{domain.NodeRoleCompute, domain.NodeRoleStorage}, true)
	if len(roles) != 3 || roles[2] != domain.NodeRoleManagement {
		t.Fatalf("management role was not added: %#v", roles)
	}
	roles = setManagementRole(roles, false)
	if len(roles) != 2 || roles[0] != domain.NodeRoleCompute || roles[1] != domain.NodeRoleStorage {
		t.Fatalf("management role removal changed other roles: %#v", roles)
	}
}
