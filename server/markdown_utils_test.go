package main

import (
	"testing"
)

func TestConvertMarkdownToHTML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "bold with asterisks",
			input:    "This is **bold** text",
			expected: "This is <strong>bold</strong> text",
		},
		{
			name:     "bold with underscores",
			input:    "This is __bold__ text",
			expected: "This is <strong>bold</strong> text",
		},
		{
			name:     "italic with asterisks",
			input:    "This is *italic* text",
			expected: "This is <em>italic</em> text",
		},
		{
			name:     "italic with underscores",
			input:    "This is _italic_ text",
			expected: "This is <em>italic</em> text",
		},
		{
			name:     "strikethrough",
			input:    "This is ~~strikethrough~~ text",
			expected: "This is <del>strikethrough</del> text",
		},
		{
			name:     "inline code",
			input:    "This is `code` text",
			expected: "This is <code>code</code> text",
		},
		{
			name:     "code block",
			input:    "```\ncode block\n```",
			expected: "<pre><code><br>code block<br></code></pre>",
		},
		{
			name:     "code block with language",
			input:    "```javascript\nconsole.log('hello');\n```",
			expected: "<pre><code class=\"language-javascript\">console.log(&#39;hello&#39;);<br></code></pre>",
		},
		{
			name:     "link",
			input:    "Check out [this link](https://example.com)",
			expected: "Check out <a href=\"https://example.com\">this link</a>",
		},
		{
			name:     "line breaks",
			input:    "Line 1\nLine 2",
			expected: "Line 1<br>Line 2",
		},
		{
			name:     "paragraph breaks (reduced spacing)",
			input:    "Paragraph 1\n\nParagraph 2",
			expected: "Paragraph 1<br>Paragraph 2",
		},
		{
			name:     "mixed formatting",
			input:    "**Bold** and *italic* with `code`",
			expected: "<strong>Bold</strong> and <em>italic</em> with <code>code</code>",
		},
		{
			name:     "html escaping",
			input:    "This has <script>alert('xss')</script> tags",
			expected: "This has &lt;script&gt;alert(&#39;xss&#39;)&lt;/script&gt; tags",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "plain text",
			input:    "Just plain text",
			expected: "Just plain text",
		},
		{
			name:     "simple table with header",
			input:    "| Name | Age |\n|------|-----|\n| John | 30 |\n| Jane | 25 |",
			expected: "<table><thead><tr><th>Name</th><th>Age</th></tr></thead><tbody><tr><td>John</td><td>30</td></tr><tr><td>Jane</td><td>25</td></tr></tbody></table>",
		},
		{
			name:     "table without header separator",
			input:    "| Name | Age |\n| John | 30 |\n| Jane | 25 |",
			expected: "<table><tbody><tr><td>Name</td><td>Age</td></tr><tr><td>John</td><td>30</td></tr><tr><td>Jane</td><td>25</td></tr></tbody></table>",
		},
		{
			name:     "table with alignment",
			input:    "| Left | Center | Right |\n|:-----|:------:|------:|\n| L1 | C1 | R1 |",
			expected: "<table><thead><tr><th>Left</th><th>Center</th><th>Right</th></tr></thead><tbody><tr><td>L1</td><td>C1</td><td>R1</td></tr></tbody></table>",
		},
		{
			name:     "table mixed with text",
			input:    "Before table\n| Col1 | Col2 |\n|------|------|\n| A | B |\nAfter table",
			expected: "Before table<br><table><thead><tr><th>Col1</th><th>Col2</th></tr></thead><tbody><tr><td>A</td><td>B</td></tr></tbody></table><br>After table",
		},
		{
			name:     "heading h1",
			input:    "# Main Heading",
			expected: "<h1>Main Heading</h1>",
		},
		{
			name:     "heading h2",
			input:    "## Subheading",
			expected: "<h2>Subheading</h2>",
		},
		{
			name:     "heading h3",
			input:    "### Section",
			expected: "<h3>Section</h3>",
		},
		{
			name:     "heading h6 max",
			input:    "###### Deepest Level",
			expected: "<h6>Deepest Level</h6>",
		},
		{
			name:     "invalid heading without space",
			input:    "##NoSpace",
			expected: "##NoSpace",
		},
		{
			name:     "headings mixed with text",
			input:    "Some text\n# Heading\nMore text",
			expected: "Some text<br><h1>Heading</h1><br>More text",
		},
		{
			name:     "multiple headings",
			input:    "# Title\n## Section 1\n### Subsection",
			expected: "<h1>Title</h1><br><h2>Section 1</h2><br><h3>Subsection</h3>",
		},
		{
			name:     "comprehensive markdown features",
			input:    "# Main Title\n\nThis is **bold text** and *italic text* with ~~strikethrough~~.\n\n## Code Examples\n\nInline `code snippet` and a code block:\n\n```javascript\nconsole.log('Hello World');\n```\n\n### Links and Tables\n\nCheck out [this link](https://example.com) for more info.\n\n| Feature | Status | Notes |\n|---------|--------|---------|\n| **Bold** | ✅ | *Working* |\n| `Code` | ✅ | ~~Fixed~~ Done |\n\nEnd of document.",
			expected: "<h1>Main Title</h1><br>This is <strong>bold text</strong> and <em>italic text</em> with <del>strikethrough</del>.<br><h2>Code Examples</h2><br>Inline <code>code snippet</code> and a code block:<br><pre><code class=\"language-javascript\">console.log(&#39;Hello World&#39;);<br></code></pre><br><h3>Links and Tables</h3><br>Check out <a href=\"https://example.com\">this link</a> for more info.<br><table><thead><tr><th>Feature</th><th>Status</th><th>Notes</th></tr></thead><tbody><tr><td><strong>Bold</strong></td><td>✅</td><td><em>Working</em></td></tr><tr><td><code>Code</code></td><td>✅</td><td><del>Fixed</del> Done</td></tr></tbody></table><br>End of document.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertMarkdownToHTML(tt.input)
			if result != tt.expected {
				t.Errorf("convertMarkdownToHTML() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestConvertMattermostToMatrix(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		expectedText string
		expectedHTML string
	}{
		{
			name:         "formatted content",
			input:        "This is **bold** text",
			expectedText: "This is **bold** text",
			expectedHTML: "This is <strong>bold</strong> text",
		},
		{
			name:         "plain text",
			input:        "Just plain text",
			expectedText: "Just plain text",
			expectedHTML: "",
		},
		{
			name:         "empty string",
			input:        "",
			expectedText: "",
			expectedHTML: "",
		},
		{
			name:         "mixed formatting",
			input:        "**Bold** and *italic* with [link](https://example.com)",
			expectedText: "**Bold** and *italic* with [link](https://example.com)",
			expectedHTML: "<strong>Bold</strong> and <em>italic</em> with <a href=\"https://example.com\">link</a>",
		},
		{
			name:         "table with formatting",
			input:        "| Name | Status |\n|------|--------|\n| **John** | *Active* |",
			expectedText: "| Name | Status |\n|------|--------|\n| **John** | *Active* |",
			expectedHTML: "<table><thead><tr><th>Name</th><th>Status</th></tr></thead><tbody><tr><td><strong>John</strong></td><td><em>Active</em></td></tr></tbody></table>",
		},
		{
			name:         "heading",
			input:        "# Welcome\nThis is content",
			expectedText: "# Welcome\nThis is content",
			expectedHTML: "<h1>Welcome</h1><br>This is content",
		},
		{
			name:         "comprehensive markdown in convertMattermostToMatrix",
			input:        "# Documentation\n\n**Important:** This shows *all* features:\n\n- `inline code`\n- [Links](https://example.com)\n- ~~Deprecated~~ items\n\n```go\nfmt.Println(\"Hello\")\n```\n\n| Feature | Working |\n|---------|----------|\n| **All** | *Yes* |",
			expectedText: "# Documentation\n\n**Important:** This shows *all* features:\n\n- `inline code`\n- [Links](https://example.com)\n- ~~Deprecated~~ items\n\n```go\nfmt.Println(\"Hello\")\n```\n\n| Feature | Working |\n|---------|----------|\n| **All** | *Yes* |",
			expectedHTML: "<h1>Documentation</h1><br><strong>Important:</strong> This shows <em>all</em> features:<br>- <code>inline code</code><br>- <a href=\"https://example.com\">Links</a><br>- <del>Deprecated</del> items<br><pre><code class=\"language-go\">fmt.Println(&#34;Hello&#34;)<br></code></pre><br><table><thead><tr><th>Feature</th><th>Working</th></tr></thead><tbody><tr><td><strong>All</strong></td><td><em>Yes</em></td></tr></tbody></table>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plainText, htmlContent := convertMattermostToMatrix(tt.input)
			if plainText != tt.expectedText {
				t.Errorf("convertMattermostToMatrix() plainText = %q, want %q", plainText, tt.expectedText)
			}
			if htmlContent != tt.expectedHTML {
				t.Errorf("convertMattermostToMatrix() htmlContent = %q, want %q", htmlContent, tt.expectedHTML)
			}
		})
	}
}

func TestIsValidURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected bool
	}{
		{
			name:     "https URL",
			url:      "https://example.com",
			expected: true,
		},
		{
			name:     "http URL",
			url:      "http://example.com",
			expected: true,
		},
		{
			name:     "ftp URL",
			url:      "ftp://files.example.com",
			expected: true,
		},
		{
			name:     "mailto URL",
			url:      "mailto:test@example.com",
			expected: true,
		},
		{
			name:     "relative URL",
			url:      "/path/to/page",
			expected: true,
		},
		{
			name:     "javascript protocol",
			url:      "javascript:alert('xss')",
			expected: false,
		},
		{
			name:     "data protocol",
			url:      "data:text/html,<script>alert('xss')</script>",
			expected: false,
		},
		{
			name:     "empty URL",
			url:      "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isValidURL(tt.url)
			if result != tt.expected {
				t.Errorf("isValidURL(%q) = %v, want %v", tt.url, result, tt.expected)
			}
		})
	}
}
