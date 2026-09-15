//go:build linux

package agent

import "testing"

func TestParseLinuxIDList(t *testing.T) {
	values, err := parseLinuxIDList("0-2,7")
	if err != nil || len(values) != 4 || !values[0] || !values[2] || !values[7] {
		t.Fatalf("unexpected parsed Linux ID list: %#v err=%v", values, err)
	}
	for _, invalid := range []string{"2-1", "a", "1,,2", "1-"} {
		if _, err := parseLinuxIDList(invalid); err == nil {
			t.Fatalf("invalid Linux ID list %q was accepted", invalid)
		}
	}
}

func TestEnvironmentWithDevices(t *testing.T) {
	environment := environmentWithDevices(map[string]string{"USER_VALUE": "kept"}, map[string][]string{
		"nvidia.com/gpu": {"/dev/nvidia2", "/dev/nvidia0"},
	})
	if environment["USER_VALUE"] != "kept" || environment["CUDA_VISIBLE_DEVICES"] != "2,0" ||
		environment["XIRANG_ASSIGNED_DEVICES"] == "" {
		t.Fatalf("unexpected device environment: %#v", environment)
	}
}
