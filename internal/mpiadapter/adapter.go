// Package mpiadapter provides a fail-closed vendor adapter shim. It does not
// impersonate PMIx or PMI: an administrator must configure a separately
// certified backend that owns the vendor process-manager integration.
package mpiadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const AdapterVersion = "xirang-mpi-adapter-v1"
const BackendVersion = "xirang-mpi-backend-v1"

type allocationManifest struct {
	Version          string `json:"version"`
	AllocationID     string `json:"allocationId"`
	JobID            string `json:"jobId"`
	AssignmentID     string `json:"assignmentId"`
	Attempt          uint32 `json:"attempt"`
	Launcher         string `json:"launcher"`
	NodeRank         uint32 `json:"nodeRank"`
	RankStart        uint32 `json:"rankStart"`
	RankCount        uint32 `json:"rankCount"`
	WorldSize        uint32 `json:"worldSize"`
	NetworkInterface string `json:"networkInterface"`
	PrimaryAddress   string `json:"primaryAddress"`
	Hosts            []struct {
		NodeID           string `json:"nodeId"`
		Hostname         string `json:"hostname"`
		Address          string `json:"address"`
		NetworkInterface string `json:"networkInterface"`
		Slots            int32  `json:"slots"`
	} `json:"hosts"`
}

func Runtime(program string) (string, error) {
	base := filepath.Base(program)
	for _, runtime := range []string{"openmpi", "mpich"} {
		if base == "xirang-mpi-adapter-"+runtime {
			return runtime, nil
		}
	}
	return "", errors.New("adapter must be installed as xirang-mpi-adapter-openmpi or xirang-mpi-adapter-mpich")
}

func Run(program string, arguments []string) error {
	runtime, err := Runtime(program)
	if err != nil {
		return err
	}
	backend, err := certifiedBackend(runtime)
	if err != nil {
		return err
	}
	if len(arguments) == 1 && arguments[0] == "--xirang-probe" {
		fmt.Printf("%s %s\n", AdapterVersion, runtime)
		return nil
	}
	if err := validateInvocation(runtime, arguments); err != nil {
		return err
	}
	return syscall.Exec(backend, append([]string{backend}, arguments...), os.Environ())
}

func certifiedBackend(runtime string) (string, error) {
	name := "XIRANG_MPI_" + strings.ToUpper(runtime) + "_BACKEND"
	backend := strings.TrimSpace(os.Getenv(name))
	if backend == "" || !filepath.IsAbs(backend) || filepath.Clean(backend) != backend {
		return "", fmt.Errorf("%s must name an absolute certified backend", name)
	}
	resolved, err := filepath.EvalSymlinks(backend)
	if err != nil {
		return "", fmt.Errorf("resolve MPI backend: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("MPI backend must be a non-writable executable regular file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, resolved, "--xirang-probe").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != BackendVersion+" "+runtime {
		return "", errors.New("MPI backend failed the exact XIRANG v1 certification probe")
	}
	return resolved, nil
}

func validateInvocation(runtime string, arguments []string) error {
	if len(arguments) < 6 || arguments[0] != "--xirang-manifest" || arguments[2] != "--xirang-local-rank" || arguments[4] != "--" {
		return errors.New("invalid MPI adapter invocation")
	}
	manifestPath := arguments[1]
	if !filepath.IsAbs(manifestPath) || filepath.Base(manifestPath) != "allocation.json" || filepath.Base(filepath.Dir(manifestPath)) != ".xirang" {
		return errors.New("allocation manifest path is invalid")
	}
	info, err := os.Stat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > 1<<20 {
		return errors.New("allocation manifest must be a private bounded regular file")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest allocationManifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return errors.New("allocation manifest is invalid")
	}
	localRank, err := strconv.ParseUint(arguments[3], 10, 32)
	if err != nil || manifest.Version != "xirang-allocation-v1" || manifest.Launcher != runtime ||
		manifest.AllocationID == "" || manifest.RankCount == 0 || manifest.WorldSize == 0 ||
		uint64(manifest.RankStart)+uint64(manifest.RankCount) > uint64(manifest.WorldSize) || uint32(localRank) >= manifest.RankCount || len(manifest.Hosts) == 0 {
		return errors.New("allocation manifest does not match the MPI invocation")
	}
	if arguments[5] == "" || !filepath.IsAbs(arguments[5]) {
		return errors.New("MPI target must be an absolute executable path")
	}
	for _, argument := range arguments[5:] {
		if argument == "" || len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
			return errors.New("MPI target argument is invalid")
		}
	}
	return nil
}
