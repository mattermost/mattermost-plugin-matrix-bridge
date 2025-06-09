// Package main provides a tool to generate emoji mapping tables from Mattermost's source code.
package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Println("Usage: go run emoji_generator.go <output_file>")
		os.Exit(1)
	}

	outputFile := os.Args[1]

	// Ensure the output directory exists
	outputDir := strings.TrimSuffix(outputFile, "/"+strings.Split(outputFile, "/")[len(strings.Split(outputFile, "/"))-1])
	if outputDir != outputFile {
		err := os.MkdirAll(outputDir, 0755)
		if err != nil {
			fmt.Printf("Error creating output directory: %v\n", err)
			os.Exit(1)
		}
	}

	// Download the TypeScript file
	fmt.Println("Downloading Mattermost emoji mapping file...")
	resp, err := http.Get("https://raw.githubusercontent.com/mattermost/mattermost/master/webapp/channels/src/utils/emoji.ts")
	if err != nil {
		fmt.Printf("Error downloading file: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read the content
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading response: %v\n", err)
		os.Exit(1)
	}

	// Parse the mappings
	fmt.Println("Parsing emoji mappings...")
	aliasToIndex, indexToUnicode := parseEmojiMappings(string(content))

	fmt.Printf("Found %d alias mappings and %d unicode mappings\n", len(aliasToIndex), len(indexToUnicode))

	// Generate Go file
	fmt.Printf("Generating Go file: %s\n", outputFile)
	err = generateGoFile(outputFile, aliasToIndex, indexToUnicode)
	if err != nil {
		fmt.Printf("Error generating Go file: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Successfully generated emoji mappings!")
}

func parseEmojiMappings(content string) (map[string]int, map[int]string) {
	aliasToIndex := make(map[string]int)
	indexToUnicode := make(map[int]string)

	// Find and extract EmojiIndicesByAlias section
	aliasStartIdx := strings.Index(content, "export const EmojiIndicesByAlias = new Map([")
	if aliasStartIdx != -1 {
		aliasEndIdx := strings.Index(content[aliasStartIdx:], "]);")
		if aliasEndIdx != -1 {
			aliasSection := content[aliasStartIdx : aliasStartIdx+aliasEndIdx]

			// Parse alias mappings
			aliasRegex := regexp.MustCompile(`\["([^"]+)",\s*(\d+)\]`)
			aliasMatches := aliasRegex.FindAllStringSubmatch(aliasSection, -1)

			for _, match := range aliasMatches {
				if len(match) == 3 {
					alias := match[1]
					index, err := strconv.Atoi(match[2])
					if err == nil {
						aliasToIndex[alias] = index
					}
				}
			}
		}
	}

	// Find and extract EmojiIndicesByUnicode section
	unicodeStartIdx := strings.Index(content, "export const EmojiIndicesByUnicode = new Map([")
	if unicodeStartIdx != -1 {
		unicodeEndIdx := strings.Index(content[unicodeStartIdx:], "]);")
		if unicodeEndIdx != -1 {
			unicodeSection := content[unicodeStartIdx : unicodeStartIdx+unicodeEndIdx]

			// Parse unicode mappings
			unicodeRegex := regexp.MustCompile(`\["([^"]+)",\s*(\d+)\]`)
			unicodeMatches := unicodeRegex.FindAllStringSubmatch(unicodeSection, -1)

			for _, match := range unicodeMatches {
				if len(match) == 3 {
					unicode := match[1]
					index, err := strconv.Atoi(match[2])
					if err == nil {
						indexToUnicode[index] = unicode
					}
				}
			}
		}
	}

	return aliasToIndex, indexToUnicode
}

func generateGoFile(filename string, aliasToIndex map[string]int, indexToUnicode map[int]string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	writer := bufio.NewWriter(file)
	defer func() { _ = writer.Flush() }()

	// Write package header
	_, _ = writer.WriteString("package main\n\n")
	_, _ = writer.WriteString("// This file is auto-generated from Mattermost's emoji mapping data\n")
	_, _ = writer.WriteString("// Do not edit manually - regenerate using tools/emoji_generator.go\n\n")

	// Write emojiNameToIndex map
	_, _ = writer.WriteString("// emojiNameToIndex maps emoji names to indices (from EmojiIndicesByAlias)\n")
	_, _ = writer.WriteString("var emojiNameToIndex = map[string]int{\n")

	for alias, index := range aliasToIndex {
		// Escape the alias if it contains special characters
		escapedAlias := strings.ReplaceAll(alias, `"`, `\"`)
		_, _ = fmt.Fprintf(writer, "\t\"%s\": %d,\n", escapedAlias, index)
	}

	_, _ = writer.WriteString("}\n\n")

	// Write emojiIndexToUnicode map
	_, _ = writer.WriteString("// emojiIndexToUnicode maps indices to Unicode hex codes (from EmojiIndicesByUnicode)\n")
	_, _ = writer.WriteString("var emojiIndexToUnicode = map[int]string{\n")

	for index, unicode := range indexToUnicode {
		// Escape the unicode if it contains special characters
		escapedUnicode := strings.ReplaceAll(unicode, `"`, `\"`)
		_, _ = fmt.Fprintf(writer, "\t%d: \"%s\",\n", index, escapedUnicode)
	}

	_, _ = writer.WriteString("}\n")

	return nil
}
