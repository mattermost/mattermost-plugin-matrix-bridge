package main

import (
	"strings"
	"testing"
)

func TestEmojiConversion(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "smile emoji",
			input:    "smile",
			expected: "😄",
		},
		{
			name:     "smile emoji with colons",
			input:    ":smile:",
			expected: "😄",
		},
		{
			name:     "thumbsup emoji",
			input:    "+1",
			expected: "👍",
		},
		{
			name:     "heart emoji",
			input:    "heart",
			expected: "❤️",
		},
		{
			name:     "fire emoji",
			input:    "fire",
			expected: "🔥",
		},
		{
			name:     "thinking face emoji",
			input:    "thinking_face",
			expected: "🤔",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Remove colons if present
			cleanName := strings.Trim(tt.input, ":")

			// Get emoji index from name
			index, exists := emojiNameToIndex[cleanName]
			if !exists {
				t.Errorf("Emoji name %q not found in mapping", cleanName)
				return
			}

			// Get Unicode code point from index
			unicodeHex, exists := emojiIndexToUnicode[index]
			if !exists {
				t.Errorf("Unicode mapping for index %d not found", index)
				return
			}

			// Convert hex string to Unicode character
			result := hexToUnicode(unicodeHex)
			if result != tt.expected {
				t.Errorf("Emoji conversion for %q = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestHexToUnicode(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple emoji",
			input:    "1f600",
			expected: "😀",
		},
		{
			name:     "emoji with variation selector",
			input:    "2764-fe0f",
			expected: "❤️",
		},
		{
			name:     "complex emoji sequence",
			input:    "1f441-fe0f-200d-1f5e8-fe0f",
			expected: "👁️‍🗨️",
		},
		{
			name:     "empty input",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hexToUnicode(tt.input)
			if result != tt.expected {
				t.Errorf("hexToUnicode(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
