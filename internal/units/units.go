// Package units parses and formats human-readable byte sizes.
package units

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var sizePattern = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*([kmgtp]?i?b?)\s*$`)

var multipliers = map[string]float64{
	"":    1,
	"b":   1,
	"k":   1 << 10,
	"kb":  1 << 10,
	"kib": 1 << 10,
	"m":   1 << 20,
	"mb":  1 << 20,
	"mib": 1 << 20,
	"g":   1 << 30,
	"gb":  1 << 30,
	"gib": 1 << 30,
	"t":   1 << 40,
	"tb":  1 << 40,
	"tib": 1 << 40,
	"p":   1 << 50,
	"pb":  1 << 50,
	"pib": 1 << 50,
}

// ParseBytes accepts values such as "20GB", "1.5 TiB", "512 mb", or a raw
// byte count. Binary units are used (1 KB = 1024 bytes).
func ParseBytes(value string) (int64, error) {
	match := sizePattern.FindStringSubmatch(value)
	if match == nil {
		return 0, fmt.Errorf("invalid size %q (examples: 500MB, 20GB, 1.5TB)", value)
	}

	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", value, err)
	}
	multiplier, ok := multipliers[strings.ToLower(match[2])]
	if !ok {
		return 0, fmt.Errorf("unsupported size unit %q", match[2])
	}

	bytes := number * multiplier
	if bytes < 1 || bytes > math.MaxInt64 {
		return 0, errors.New("size must be between 1 byte and 8 EiB")
	}
	if math.Abs(bytes-math.Round(bytes)) > 0.000001 {
		return 0, fmt.Errorf("size %q does not resolve to a whole number of bytes", value)
	}
	return int64(math.Round(bytes)), nil
}

// FormatBytes returns a compact binary-unit representation, for example
// "1.4 GB" or "12.0 MB". It is intended for display, not round trips.
func FormatBytes(bytes int64) string {
	if bytes < 0 {
		return "0 B"
	}
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}

	units := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	value := float64(bytes)
	for _, unit := range units {
		value /= 1024
		if value < 1024 {
			if value >= 100 {
				return fmt.Sprintf("%.0f %s", value, unit)
			}
			if value >= 10 {
				return fmt.Sprintf("%.1f %s", value, unit)
			}
			return fmt.Sprintf("%.2f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.2f EB", value/1024)
}
