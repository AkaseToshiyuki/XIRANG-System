// Package jobspec parses the intentionally small XIRANG job file format.
package jobspec

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

type Spec struct {
	ProjectID        string
	Queue            string
	Priority         int32
	MaxAttempts      int32
	Artifact         domain.NativeArtifact
	Application      string
	Arguments        []string
	WorkingDirectory string
	Environment      map[string]string
	Resources        domain.ResourceVector
	Parallel         domain.ParallelSpec
	Timeout          time.Duration
	ArrayIndices     []int32
	BatchSpaceID     string
	BatchPath        string
	BatchArgument    string
	BatchNodes       int
	Dependencies     []domain.JobDependency
}

func Parse(reader io.Reader) (Spec, error) {
	spec := Spec{
		ProjectID: "default", Queue: "default", MaxAttempts: 1,
		Environment: map[string]string{}, Resources: domain.ResourceVector{Devices: map[string]int64{}},
		Parallel: domain.ParallelSpec{Nodes: 1, TasksPerNode: 1, Launcher: "native"},
	}
	scanner := bufio.NewScanner(io.LimitReader(reader, 1<<20))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.EqualFold(line, "XIRANG 1") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Spec{}, fmt.Errorf("line %d: expected key = value", lineNumber)
		}
		key, value = strings.TrimSpace(key), unquote(strings.TrimSpace(value))
		if err := apply(&spec, key, value); err != nil {
			return Spec{}, fmt.Errorf("line %d: %w", lineNumber, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return Spec{}, err
	}
	directArtifact := spec.Artifact.URI != "" || spec.Artifact.SHA256 != "" || spec.Artifact.Architecture != "" || spec.Artifact.Entrypoint != ""
	if (spec.Application == "") == !directArtifact {
		return Spec{}, errors.New("declare either application or artifact, sha256, architecture, and entrypoint")
	}
	if directArtifact && (spec.Artifact.URI == "" || spec.Artifact.SHA256 == "" || spec.Artifact.Architecture == "" || spec.Artifact.Entrypoint == "") {
		return Spec{}, errors.New("artifact, sha256, architecture, and entrypoint must be declared together")
	}
	batch := spec.BatchSpaceID != "" || spec.BatchPath != "" || spec.BatchArgument != "" || spec.BatchNodes != 0
	if batch && (spec.BatchSpaceID == "" || spec.BatchPath == "" || spec.BatchNodes < 1) {
		return Spec{}, errors.New("batch_space, batch_file, and batch_nodes are required together")
	}
	if batch && len(spec.ArrayIndices) > 0 {
		return Spec{}, errors.New("batch_file and array cannot be combined")
	}
	if batch && spec.Parallel.Effective().Nodes != 1 {
		return Spec{}, errors.New("batch_file creates one-node child jobs; nodes must remain 1")
	}
	if spec.Resources.MilliCPU < 10 || spec.Resources.MemoryBytes <= 0 {
		return Spec{}, errors.New("cpu and memory must be declared")
	}
	return spec, nil
}

func apply(spec *Spec, key, value string) error {
	switch {
	case key == "project":
		spec.ProjectID = value
	case key == "queue":
		spec.Queue = value
	case key == "artifact":
		spec.Artifact.URI = value
	case key == "application":
		spec.Application = value
	case key == "sha256":
		spec.Artifact.SHA256 = value
	case key == "architecture":
		spec.Artifact.Architecture = value
	case key == "abi":
		spec.Artifact.ABI = value
	case key == "entrypoint":
		spec.Artifact.Entrypoint = value
	case key == "workdir":
		spec.WorkingDirectory = value
	case key == "arg":
		spec.Arguments = append(spec.Arguments, value)
	case strings.HasPrefix(key, "env."):
		name := strings.TrimPrefix(key, "env.")
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return errors.New("invalid environment variable name")
		}
		spec.Environment[name] = value
	case key == "priority":
		number, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			return errors.New("priority must be an integer")
		}
		spec.Priority = int32(number)
	case key == "attempts":
		number, err := strconv.ParseInt(value, 10, 32)
		if err != nil || number < 1 || number > 100 {
			return errors.New("attempts must be between 1 and 100")
		}
		spec.MaxAttempts = int32(number)
	case key == "array":
		indices, err := parseArray(value)
		if err != nil {
			return err
		}
		spec.ArrayIndices = indices
	case key == "batch_space":
		spec.BatchSpaceID = value
	case key == "batch_file":
		spec.BatchPath = value
	case key == "batch_arg":
		spec.BatchArgument = value
	case key == "batch_nodes":
		number, err := strconv.ParseInt(value, 10, 32)
		if err != nil || number < 1 || number > 128 {
			return errors.New("batch_nodes must be between 1 and 128")
		}
		spec.BatchNodes = int(number)
	case key == "depends":
		dependency, err := parseDependency(value)
		if err != nil {
			return err
		}
		spec.Dependencies = append(spec.Dependencies, dependency)
	case key == "cpu":
		number, err := parseCPU(value)
		if err != nil {
			return err
		}
		spec.Resources.MilliCPU = number
	case key == "memory":
		number, err := parseBytes(value)
		if err != nil {
			return err
		}
		spec.Resources.MemoryBytes = number
	case key == "nodes":
		number, err := strconv.ParseInt(value, 10, 32)
		if err != nil || number < 1 || number > 128 {
			return errors.New("nodes must be between 1 and 128")
		}
		spec.Parallel.Nodes = int32(number)
	case key == "tasks_per_node":
		number, err := strconv.ParseInt(value, 10, 32)
		if err != nil || number < 1 || number > 256 {
			return errors.New("tasks_per_node must be between 1 and 256")
		}
		spec.Parallel.TasksPerNode = int32(number)
	case key == "launcher":
		launcher := strings.ToLower(strings.TrimSpace(value))
		if launcher != "native" && launcher != "openmpi" && launcher != "mpich" && launcher != "intelmpi" {
			return errors.New("launcher must be native, openmpi, mpich, or intelmpi")
		}
		spec.Parallel.Launcher = launcher
	case key == "storage":
		return errors.New("storage is managed by the administrator quota and cannot be declared by a job")
	case strings.HasPrefix(key, "device."):
		name := strings.TrimPrefix(key, "device.")
		if !validDeviceResourceName(name) {
			return errors.New("device resource name is invalid")
		}
		number, err := strconv.ParseInt(value, 10, 64)
		if err != nil || number < 1 || number > 1_000_000 {
			return errors.New("device quantity must be between 1 and 1000000")
		}
		spec.Resources.Devices[name] = number
	case key == "timeout":
		duration, err := parseJobDuration(value)
		if err != nil || duration < 0 {
			return errors.New("timeout must be a positive duration such as 12h, 7d, or 30d")
		}
		spec.Timeout = duration
	default:
		return fmt.Errorf("unknown field %q", key)
	}
	if int64(spec.Parallel.Nodes)*int64(spec.Parallel.TasksPerNode) > 4096 {
		return errors.New("parallel jobs may contain at most 4096 ranks")
	}
	return nil
}

func parseJobDuration(value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) > 1 && (trimmed[len(trimmed)-1] == 'd' || trimmed[len(trimmed)-1] == 'D') {
		days, err := strconv.ParseFloat(strings.TrimSpace(trimmed[:len(trimmed)-1]), 64)
		if err != nil || days < 0 || days > float64(math.MaxInt64)/float64(24*time.Hour) {
			return 0, errors.New("invalid day duration")
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(trimmed)
}

func parseArray(value string) ([]int32, error) {
	startText, endText, ok := strings.Cut(strings.TrimSpace(value), "-")
	if !ok {
		return nil, errors.New("array must use START-END (for example 0-15)")
	}
	start, startErr := strconv.ParseInt(strings.TrimSpace(startText), 10, 32)
	end, endErr := strconv.ParseInt(strings.TrimSpace(endText), 10, 32)
	if startErr != nil || endErr != nil || start < 0 || end < start || end-start+1 > 1000 {
		return nil, errors.New("array must contain between 1 and 1000 non-negative indices")
	}
	result := make([]int32, 0, end-start+1)
	for index := start; index <= end; index++ {
		result = append(result, int32(index))
	}
	return result, nil
}

func parseDependency(value string) (domain.JobDependency, error) {
	jobID, condition, found := strings.Cut(strings.TrimSpace(value), ":")
	if !found {
		condition = "afterok"
	}
	condition = strings.ToLower(strings.TrimSpace(condition))
	if jobID == "" || len(jobID) > 256 || (condition != "afterok" && condition != "afterany") {
		return domain.JobDependency{}, errors.New("depends must use JOB_ID or JOB_ID:afterok|afterany")
	}
	return domain.JobDependency{JobID: jobID, Condition: condition}, nil
}

func validDeviceResourceName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._-/", character) {
			continue
		}
		return false
	}
	return true
}

func parseCPU(value string) (int64, error) {
	const maxLogicalCores int64 = 100000

	// Legacy millicore input remains readable so persisted templates and queued
	// jobs created by earlier XIRANG versions can still be resubmitted. New job
	// files use an integer logical-core count.
	if strings.HasSuffix(value, "m") {
		number, err := strconv.ParseInt(strings.TrimSuffix(value, "m"), 10, 64)
		if err != nil || number < 10 || number > maxLogicalCores*1000 {
			return 0, errors.New("cpu must be a positive integer logical-core count (for example 2)")
		}
		return number, nil
	}
	cores, err := strconv.ParseInt(value, 10, 64)
	if err != nil || cores < 1 || cores > maxLogicalCores {
		return 0, errors.New("cpu must be a positive integer logical-core count (for example 2)")
	}
	return cores * 1000, nil
}

func parseBytes(value string) (int64, error) {
	type unit struct {
		suffix     string
		multiplier int64
	}
	// M/G/T are the canonical XIRANG job-file units. The IEC spellings are
	// accepted as a migration aid for templates saved by previous releases.
	units := []unit{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
	}
	for _, unit := range units {
		suffix, multiplier := unit.suffix, unit.multiplier
		if strings.HasSuffix(value, suffix) {
			number, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(value, suffix)), 10, 64)
			if err != nil || number < 0 || number > (1<<63-1)/multiplier {
				return 0, errors.New("invalid byte quantity")
			}
			return number * multiplier, nil
		}
	}
	return 0, errors.New("byte quantity requires M, G, or T (for example 512M or 4G)")
}

func unquote(value string) string {
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}
