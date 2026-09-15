// Package runtimecompat implements the portable, fail-closed runtime contract
// shared by build finalization, scheduling, and node-local execution.
package runtimecompat

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	keyPattern     = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	versionPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*`)
)

// ValidateRequirements rejects malformed or unbounded contracts before they
// are persisted or sent to an Agent.
func ValidateRequirements(requirements map[string]string) error {
	if len(requirements) > 64 {
		return fmt.Errorf("runtime contract exceeds 64 requirements")
	}
	for key, expression := range requirements {
		if !keyPattern.MatchString(key) || len(expression) == 0 || len(expression) > 256 || strings.TrimSpace(expression) != expression {
			return fmt.Errorf("invalid runtime requirement %q", key)
		}
		op, operand := splitExpression(expression)
		if operand == "" {
			return fmt.Errorf("runtime requirement %q has an empty value", key)
		}
		if op != "=" && versionPattern.FindString(operand) != operand {
			return fmt.Errorf("runtime requirement %q uses an invalid ordered version", key)
		}
	}
	return nil
}

// ValidateFacts applies the same bounds to node-reported facts. Fact values
// are opaque unless a requirement asks for an ordered version comparison.
func ValidateFacts(facts map[string]string) error {
	if len(facts) > 64 {
		return fmt.Errorf("runtime facts exceed 64 entries")
	}
	for key, value := range facts {
		if !keyPattern.MatchString(key) || len(value) == 0 || len(value) > 256 || strings.TrimSpace(value) != value {
			return fmt.Errorf("invalid runtime fact %q", key)
		}
	}
	return nil
}

// Compatible returns false when a required fact is missing, malformed for an
// ordered comparison, or does not satisfy its expression. Empty contracts are
// intentionally accepted for artifacts produced before this field existed.
func Compatible(requirements, facts map[string]string) (bool, string) {
	if err := ValidateRequirements(requirements); err != nil {
		return false, err.Error()
	}
	if err := ValidateFacts(facts); err != nil {
		return false, err.Error()
	}
	for key, expression := range requirements {
		actual, ok := facts[key]
		if !ok {
			return false, fmt.Sprintf("required runtime fact %q is missing", key)
		}
		op, expected := splitExpression(expression)
		if op == "=" {
			if actual != expected {
				return false, fmt.Sprintf("runtime fact %q is %q, requires %q", key, actual, expected)
			}
			continue
		}
		comparison, ok := compareVersions(actual, expected)
		if !ok || !orderedMatch(op, comparison) {
			return false, fmt.Sprintf("runtime fact %q is %q, requires %s%s", key, actual, op, expected)
		}
	}
	return true, ""
}

// Satisfies is the allocation-free hot-path form used while filtering many
// candidate nodes. Call Compatible when a human-readable rejection reason is
// needed at an execution boundary.
func Satisfies(requirements, facts map[string]string) bool {
	if ValidateRequirements(requirements) != nil || ValidateFacts(facts) != nil {
		return false
	}
	for key, expression := range requirements {
		actual, ok := facts[key]
		if !ok {
			return false
		}
		op, expected := splitExpression(expression)
		if op == "=" {
			if actual != expected {
				return false
			}
			continue
		}
		comparison, ok := compareVersions(actual, expected)
		if !ok || !orderedMatch(op, comparison) {
			return false
		}
	}
	return true
}

func splitExpression(expression string) (string, string) {
	for _, operator := range []string{"<=", ">=", "<", ">", "="} {
		if strings.HasPrefix(expression, operator) {
			return operator, strings.TrimSpace(strings.TrimPrefix(expression, operator))
		}
	}
	return "=", expression
}

func compareVersions(left, right string) (int, bool) {
	leftParts, leftLength, leftOK := parseVersion(left)
	rightParts, rightLength, rightOK := parseVersion(right)
	if !leftOK || !rightOK {
		return 0, false
	}
	length := leftLength
	if rightLength > length {
		length = rightLength
	}
	for index := 0; index < length; index++ {
		var leftValue, rightValue uint64
		if index < leftLength {
			leftValue = leftParts[index]
		}
		if index < rightLength {
			rightValue = rightParts[index]
		}
		if leftValue < rightValue {
			return -1, true
		}
		if leftValue > rightValue {
			return 1, true
		}
	}
	return 0, true
}

func parseVersion(text string) ([16]uint64, int, bool) {
	var parts [16]uint64
	if len(text) == 0 || text[0] < '0' || text[0] > '9' {
		return parts, 0, false
	}
	count, index := 0, 0
	for index < len(text) {
		if count == len(parts) {
			return parts, 0, false
		}
		start := index
		for index < len(text) && text[index] >= '0' && text[index] <= '9' {
			index++
		}
		value, err := strconv.ParseUint(text[start:index], 10, 64)
		if err != nil {
			return parts, 0, false
		}
		parts[count], count = value, count+1
		if index >= len(text) || text[index] != '.' {
			break
		}
		index++
		if index >= len(text) || text[index] < '0' || text[index] > '9' {
			return parts, 0, false
		}
	}
	return parts, count, true
}

func orderedMatch(operator string, comparison int) bool {
	switch operator {
	case "<":
		return comparison < 0
	case "<=":
		return comparison <= 0
	case ">":
		return comparison > 0
	case ">=":
		return comparison >= 0
	default:
		return false
	}
}
