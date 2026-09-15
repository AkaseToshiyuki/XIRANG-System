package domain

import "testing"

func TestNodeRolesAreNonExclusiveAndGateScheduling(t *testing.T) {
	legacy := Node{Status: NodeStatusReady}
	if !legacy.HasRole(NodeRoleCompute) || !legacy.Schedulable() {
		t.Fatal("legacy node should retain its compute role")
	}
	multi := Node{Status: NodeStatusReady, Roles: []NodeRole{NodeRoleManagement, NodeRoleStorage}}
	if !multi.HasRole(NodeRoleManagement) || !multi.HasRole(NodeRoleStorage) || multi.HasRole(NodeRoleCompute) || multi.Schedulable() {
		t.Fatal("explicit non-compute roles were not respected")
	}
	multi.Roles = append(multi.Roles, NodeRoleCompute)
	if !multi.Schedulable() {
		t.Fatal("roles must be non-exclusive")
	}
}

func TestRemovedNodeIsNeverSchedulable(t *testing.T) {
	node := Node{Status: NodeStatusRemoved, Roles: []NodeRole{NodeRoleCompute}}
	if node.Schedulable() {
		t.Fatal("removed node was schedulable")
	}
}

func TestNodeLifecycleTransitions(t *testing.T) {
	tests := []struct {
		from    NodeStatus
		to      NodeStatus
		allowed bool
	}{
		{NodeStatusPendingApproval, NodeStatusReady, true},
		{NodeStatusPendingApproval, NodeStatusRejected, true},
		{NodeStatusReady, NodeStatusCordoned, true},
		{NodeStatusUnreachable, NodeStatusReady, true},
		{NodeStatusRejected, NodeStatusReady, false},
		{NodeStatusReady, NodeStatusPendingApproval, false},
	}

	for _, test := range tests {
		if got := CanTransitionNode(test.from, test.to); got != test.allowed {
			t.Fatalf("transition %s -> %s: got %v, want %v", test.from, test.to, got, test.allowed)
		}
	}
}
