package main

import (
	"strconv"
	"strings"
)

// convertEmojiForMatrix converts Mattermost emoji names to Unicode characters for Matrix
// This function now uses Mattermost's comprehensive emoji mapping data
func (p *Plugin) convertEmojiForMatrix(emojiName string) string {
	// Remove colons if present
	cleanName := strings.Trim(emojiName, ":")

	// Get emoji index from name
	index, exists := emojiNameToIndex[cleanName]
	if !exists {
		p.API.LogDebug("Unknown emoji name", "emoji", cleanName)
		return ":" + cleanName + ":"
	}

	// Get Unicode code point from index
	unicodeHex, exists := emojiIndexToUnicode[index]
	if !exists {
		p.API.LogDebug("No Unicode mapping for emoji index", "emoji", cleanName, "index", index)
		return ":" + cleanName + ":"
	}

	// Convert hex string to Unicode character
	unicode := hexToUnicode(unicodeHex)
	if unicode == "" {
		p.API.LogDebug("Failed to convert hex to Unicode", "emoji", cleanName, "hex", unicodeHex)
		return ":" + cleanName + ":"
	}

	return unicode
}

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
