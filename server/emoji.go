package main

import (
	"strconv"
	"strings"
)

// hexToUnicode converts a hex string (with potential -fe0f suffixes) to Unicode character
func hexToUnicode(hexStr string) string {
	// Handle complex emoji sequences (e.g., "1f441-fe0f-200d-1f5e8-fe0f")
	parts := strings.Split(hexStr, "-")
	var result strings.Builder

	for _, part := range parts {
		// Include variation selectors and zero-width joiners for proper emoji rendering
		// fe0f = variation selector-16 (emoji presentation)
		// 200d = zero-width joiner (combines emoji)

		// Convert hex to rune
		if codePoint, err := strconv.ParseInt(part, 16, 32); err == nil && codePoint > 0 {
			result.WriteRune(rune(codePoint))
		}
	}

	return result.String()
}
