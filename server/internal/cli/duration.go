package cli

import (
	"regexp"
	"strconv"
	"time"
)

// dayUnit matches a decimal number (with optional leading digits) followed by
// `d` (days), so both "5d" and "1.5d" are captured whole and expanded to hours.
var dayUnit = regexp.MustCompile(`(\d*\.\d+|\d+)d`)

// ParseDurationWithDays accepts the standard Go time.ParseDuration syntax plus a
// `d` (day) suffix, which the stdlib rejects. "5d" → 120h, "1d12h" → 36h,
// "0.5d" → 12h. Overflow or malformed numbers propagate as errors.
func ParseDurationWithDays(value string) (time.Duration, error) {
	var convErr error
	expanded := dayUnit.ReplaceAllStringFunc(value, func(match string) string {
		days, err := strconv.ParseFloat(match[:len(match)-1], 64)
		if err != nil {
			convErr = err
			return match
		}
		// time.ParseDuration handles fractional hours natively, and rejects
		// overflow on its own.
		return strconv.FormatFloat(days*24, 'f', -1, 64) + "h"
	})
	if convErr != nil {
		return 0, convErr
	}
	return time.ParseDuration(expanded)
}
