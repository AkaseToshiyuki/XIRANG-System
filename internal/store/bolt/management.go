package bolt

import (
	"encoding/json"
	"fmt"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"go.etcd.io/bbolt"
)

// Enforce the two management slots inside the node's write transaction, so
// concurrent configuration requests cannot each see a free final slot.
func validateManagementAssignment(bucket *bbolt.Bucket, previous domain.Node, node *domain.Node) error {
	if !node.HasRole(domain.NodeRoleManagement) {
		// Keep a disabling node's slot reserved until the Agent acknowledges it.
		if node.PendingManagementCommandID == "" {
			node.ManagementRole = ""
		}
		return nil
	}
	if previous.HasRole(domain.NodeRoleManagement) && previous.ManagementRole == node.ManagementRole {
		return nil
	}
	if node.ManagementRole != "" && node.ManagementRole != "primary" && node.ManagementRole != "backup" {
		return enrollment.ErrInvalidRequest
	}
	occupied := map[string]bool{}
	count := 0
	err := bucket.ForEach(func(_, data []byte) error {
		var record nodeRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return err
		}
		other := record.Node
		if other.ID == node.ID || other.RemovedAt != nil || other.Status == domain.NodeStatusRemoved || (!other.HasRole(domain.NodeRoleManagement) && other.PendingManagementCommandID == "") {
			return nil
		}
		count++
		role := other.ManagementRole
		if role == "" {
			if other.ManagementPanelStatus == "controller" {
				role = "primary"
			} else {
				role = "backup"
			}
		}
		occupied[role] = true
		return nil
	})
	if err != nil {
		return err
	}
	if count >= 2 {
		return fmt.Errorf("%w: at most one primary and one backup management node are allowed", enrollment.ErrConflict)
	}
	if node.ManagementRole == "" {
		if !occupied["primary"] {
			node.ManagementRole = "primary"
		} else {
			node.ManagementRole = "backup"
		}
	}
	if occupied[node.ManagementRole] {
		return fmt.Errorf("%w: management %s is already assigned", enrollment.ErrConflict, node.ManagementRole)
	}
	return nil
}
