package main

import (
	"html"
	"regexp"
	"strings"
)

// convertMarkdownToHTML converts Mattermost-style Markdown to HTML for Matrix
func convertMarkdownToHTML(markdown string) string {
	if strings.TrimSpace(markdown) == "" {
		return ""
	}

	// Start with HTML-escaped content to prevent XSS
	htmlContent := html.EscapeString(markdown)

	// Apply markdown conversions in order of precedence
	// Process code blocks first before line breaks to preserve structure
	htmlContent = convertCodeBlocks(htmlContent)
	htmlContent = convertInlineCode(htmlContent)
	// Convert line breaks after code processing so tables can work with <br> tags
	htmlContent = convertLineBreaks(htmlContent)
	// Process headings and tables after line breaks, but they can contain other formatting
	htmlContent = convertHeadings(htmlContent)
	htmlContent = convertTablesWithFormatting(htmlContent)
	htmlContent = convertLinks(htmlContent)

	return htmlContent
}

// convertTablesWithFormatting converts Markdown tables to HTML tables and applies formatting to cell content
func convertTablesWithFormatting(content string) string {
	// First convert tables structure
	content = convertTables(content)

	// Then apply formatting to content inside table cells
	content = convertBold(content)
	content = convertItalic(content)
	content = convertStrikethrough(content)

	return content
}

// convertTables converts Markdown tables to HTML tables
func convertTables(content string) string {
	lines := strings.Split(content, "<br>")
	var result []string
	var inTable bool
	var tableRows []string

	for i, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines
		if line == "" {
			if inTable {
				// Empty line ends table
				result = append(result, buildHTMLTable(tableRows))
				inTable = false
				tableRows = []string{}
			}
			result = append(result, line)
			continue
		}

		// Check if this is a table separator line
		if isTableSeparator(line) {
			continue // Skip separator lines
		}

		// Check if this line looks like a table row (contains |)
		if strings.Contains(line, "|") && len(line) > 1 {
			// Check if next line is a separator to determine if current row is header
			isHeader := false
			if i+1 < len(lines) {
				nextLine := strings.TrimSpace(lines[i+1])
				if isTableSeparator(nextLine) {
					isHeader = true
				}
			}

			// Start new table if not in one
			if !inTable {
				inTable = true
				tableRows = []string{}
			}

			// Add the row
			tableRows = append(tableRows, processTableRow(line, isHeader))
		} else {
			// Not a table row - end table if we were in one
			if inTable {
				result = append(result, buildHTMLTable(tableRows))
				inTable = false
				tableRows = []string{}
			}
			result = append(result, line)
		}
	}

	// Handle case where content ends with a table
	if inTable && len(tableRows) > 0 {
		result = append(result, buildHTMLTable(tableRows))
	}

	return strings.Join(result, "<br>")
}

// isTableSeparator checks if a line is a table separator (like |---|---|)
func isTableSeparator(line string) bool {
	if !strings.Contains(line, "|") {
		return false
	}

	// Remove | characters and check if remaining chars are only - : and spaces
	cleaned := strings.ReplaceAll(line, "|", "")
	cleaned = strings.ReplaceAll(cleaned, " ", "")
	cleaned = strings.ReplaceAll(cleaned, "-", "")
	cleaned = strings.ReplaceAll(cleaned, ":", "")

	return len(cleaned) == 0 && strings.Contains(line, "-")
}

// processTableRow converts a markdown table row to HTML
func processTableRow(line string, isHeader bool) string {
	// Remove leading/trailing pipes and split
	line = strings.Trim(line, " |")
	cells := strings.Split(line, "|")

	var htmlCells []string
	tag := "td"
	if isHeader {
		tag = "th"
	}

	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		htmlCells = append(htmlCells, "<"+tag+">"+cell+"</"+tag+">")
	}

	return "<tr>" + strings.Join(htmlCells, "") + "</tr>"
}

// buildHTMLTable creates a complete HTML table from processed rows
func buildHTMLTable(rows []string) string {
	if len(rows) == 0 {
		return ""
	}

	var thead, tbody string
	var tbodyRows []string
	var headerRows []string

	// Separate header and body rows
	for _, row := range rows {
		if strings.Contains(row, "<th>") {
			headerRows = append(headerRows, row)
		} else {
			tbodyRows = append(tbodyRows, row)
		}
	}

	// Build thead if we have header rows
	if len(headerRows) > 0 {
		thead = "<thead>" + strings.Join(headerRows, "") + "</thead>"
	}

	// Build tbody if we have data rows
	if len(tbodyRows) > 0 {
		tbody = "<tbody>" + strings.Join(tbodyRows, "") + "</tbody>"
	}

	return "<table>" + thead + tbody + "</table>"
}

// convertCodeBlocks converts ```code``` blocks to <pre><code>
func convertCodeBlocks(content string) string {
	// Multi-line code blocks with optional language
	codeBlockRegex := regexp.MustCompile("```(?:(\\w+)\\n)?([\\s\\S]*?)```")
	return codeBlockRegex.ReplaceAllStringFunc(content, func(match string) string {
		parts := codeBlockRegex.FindStringSubmatch(match)
		if len(parts) >= 3 {
			language := parts[1]
			code := parts[2]
			if language != "" {
				return "<pre><code class=\"language-" + language + "\">" + code + "</code></pre>"
			}
			return "<pre><code>" + code + "</code></pre>"
		}
		return match
	})
}

// convertInlineCode converts `code` to <code>
func convertInlineCode(content string) string {
	inlineCodeRegex := regexp.MustCompile("`([^`]+)`")
	return inlineCodeRegex.ReplaceAllString(content, "<code>$1</code>")
}

// convertBold converts **text** and __text__ to <strong>
func convertBold(content string) string {
	// Double asterisks
	content = regexp.MustCompile(`\*\*([^\*]+)\*\*`).ReplaceAllString(content, "<strong>$1</strong>")
	// Double underscores
	content = regexp.MustCompile(`__([^_]+)__`).ReplaceAllString(content, "<strong>$1</strong>")
	return content
}

// convertItalic converts *text* and _text_ to <em> (but not if already processed as bold)
func convertItalic(content string) string {
	// Single asterisks - only match if not adjacent to other asterisks
	// This regex matches single asterisks that don't have asterisks immediately before/after
	content = regexp.MustCompile(`(^|[^\*])\*([^\*\n]+)\*([^\*]|$)`).ReplaceAllString(content, "$1<em>$2</em>$3")

	// Single underscores for italic (avoiding double underscores which are bold)
	content = regexp.MustCompile(`(^|[^_])_([^_\n]+)_([^_]|$)`).ReplaceAllString(content, "$1<em>$2</em>$3")

	return content
}

// convertStrikethrough converts ~~text~~ to <del>
func convertStrikethrough(content string) string {
	strikethroughRegex := regexp.MustCompile(`~~([^~]+)~~`)
	return strikethroughRegex.ReplaceAllString(content, "<del>$1</del>")
}

// convertLinks converts [text](url) to <a href="url">text</a>
func convertLinks(content string) string {
	linkRegex := regexp.MustCompile(`\[([^\]]+)\]\(([^\)]+)\)`)
	return linkRegex.ReplaceAllStringFunc(content, func(match string) string {
		parts := linkRegex.FindStringSubmatch(match)
		if len(parts) >= 3 {
			text := parts[1]
			url := parts[2]
			// Basic URL validation and ensure it's properly escaped
			if isValidURL(url) {
				return "<a href=\"" + html.EscapeString(url) + "\">" + text + "</a>"
			}
		}
		return match
	})
}

// convertLineBreaks converts line breaks with reduced spacing for Matrix compatibility
func convertLineBreaks(content string) string {
	// Convert double newlines (paragraph breaks) to single <br> to reduce spacing in Matrix
	content = strings.ReplaceAll(content, "\n\n", "<br>")
	// Convert remaining single newlines to <br>
	content = strings.ReplaceAll(content, "\n", "<br>")
	return content
}

// convertHeadings converts # Heading to <h1>Heading</h1>, ## to <h2>, etc.
func convertHeadings(content string) string {
	lines := strings.Split(content, "<br>")
	var result []string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Check if line starts with # (heading)
		if strings.HasPrefix(line, "#") {
			// Count the number of # characters
			headingLevel := 0
			for i := 0; i < len(line) && line[i] == '#' && headingLevel < 6; i++ {
				headingLevel++
			}

			// Check if there's a space after the # characters (proper heading format)
			if headingLevel > 0 && headingLevel < len(line) && line[headingLevel] == ' ' {
				// Extract heading text (everything after "### ")
				headingText := strings.TrimSpace(line[headingLevel+1:])
				if headingText != "" {
					// Convert to HTML heading
					result = append(result, "<h"+string(rune('0'+headingLevel))+">"+headingText+"</h"+string(rune('0'+headingLevel))+">")
					continue
				}
			}
		}

		// Not a heading, add line as-is
		result = append(result, line)
	}

	return strings.Join(result, "<br>")
}

// isValidURL performs basic URL validation
func isValidURL(url string) bool {
	if url == "" {
		return false
	}

	// Allow http, https, ftp, and mailto protocols
	validProtocols := []string{"http://", "https://", "ftp://", "mailto:"}
	for _, protocol := range validProtocols {
		if strings.HasPrefix(strings.ToLower(url), protocol) {
			return true
		}
	}

	// Allow relative URLs that don't start with javascript: or other dangerous protocols
	dangerousProtocols := []string{"javascript:", "data:", "vbscript:", "file:"}
	urlLower := strings.ToLower(url)
	for _, dangerous := range dangerousProtocols {
		if strings.HasPrefix(urlLower, dangerous) {
			return false
		}
	}

	// If it doesn't have a protocol but isn't dangerous, assume it's relative
	return !strings.Contains(url, ":")
}

// convertMattermostToMatrix converts Mattermost markdown content to Matrix format
// Returns both plain text and HTML versions
func convertMattermostToMatrix(mattermostContent string) (plainText string, htmlContent string) {
	// Plain text is the original content with some cleanup
	plainText = strings.TrimSpace(mattermostContent)

	// Convert to HTML
	htmlContent = convertMarkdownToHTML(mattermostContent)

	// If HTML conversion resulted in the same as plain text (no formatting),
	// return empty HTML to use plain text only
	if htmlContent == html.EscapeString(plainText) {
		htmlContent = ""
	}

	return plainText, htmlContent
}
