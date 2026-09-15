package domain

import "testing"

func TestResourceVectorFitsAndSubtracts(t *testing.T) {
	available := ResourceVector{
		MilliCPU:    8000,
		MemoryBytes: 16 << 30,
		Devices:     map[string]int64{"nvidia.com/gpu": 2},
	}
	requested := ResourceVector{
		MilliCPU:    2500,
		MemoryBytes: 4 << 30,
		Devices:     map[string]int64{"nvidia.com/gpu": 1},
	}

	if !available.Fits(requested) {
		t.Fatal("expected resources to fit")
	}
	remaining, err := available.Subtract(requested)
	if err != nil {
		t.Fatalf("subtract returned an error: %v", err)
	}
	if remaining.MilliCPU != 5500 || remaining.MemoryBytes != 12<<30 {
		t.Fatalf("unexpected remainder: %#v", remaining)
	}
	if remaining.Devices["nvidia.com/gpu"] != 1 {
		t.Fatalf("unexpected device remainder: %#v", remaining.Devices)
	}
}

func TestResourceVectorRejectsInvalidQuantities(t *testing.T) {
	resource := ResourceVector{MilliCPU: -1}
	if err := resource.Validate(); err == nil {
		t.Fatal("expected negative CPU to be rejected")
	}
}

func TestNodeSchedulableOnlyWhenReady(t *testing.T) {
	if !(Node{Status: NodeStatusReady}).Schedulable() {
		t.Fatal("ready node should be schedulable")
	}
	if (Node{Status: NodeStatusCordoned}).Schedulable() {
		t.Fatal("cordoned node must not be schedulable")
	}
}
