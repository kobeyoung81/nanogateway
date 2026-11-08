package gateway

import (
	"regexp"
	"strings"
)

// MarkdownToText converts markdown into plain text for channels that cannot
// render markdown (e.g. WeChat KF). It handles the constructs the agent is
// told it can emit and degrades gracefully on anything else.
func MarkdownToText(src string) string {
	text := src

	// Fenced code blocks → keep the code content, drop the fence markers.
	text = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\n(.*?)```").ReplaceAllString(text, "$1")

	// Images → alt text.
	text = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`).ReplaceAllString(text, "$1")

	// Links → keep the label, append the URL in parentheses.
	text = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`).ReplaceAllString(text, "$1 ($2)")

	// Headings → strip the # prefix.
	text = regexp.MustCompile(`(?m)^(#{1,6})\s+`).ReplaceAllString(text, "")

	// Bold / italic / strikethrough / inline code.
	text = regexp.MustCompile(`\*\*\*(.+?)\*\*\*`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`\*\*(.+?)\*\*`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`__(.+?)__`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`\*(.+?)\*`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`_(.+?)_`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`~~(.+?)~~`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile("`([^`]+)`").ReplaceAllString(text, "$1")

	// Blockquotes.
	text = regexp.MustCompile(`(?m)^>\s*`).ReplaceAllString(text, "")

	// Lists → keep the bullets but normalize markers.
	text = regexp.MustCompile(`(?m)^\s*([-*+]|\d+[.)])\s+`).ReplaceAllString(text, "• ")

	// Tables → flatten each row to " | "-joined cells.
	text = flattenTables(text)

	// Collapse 3+ blank lines to one and trim.
	text = regexp.MustCompile(`\n{3,}`).ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

func flattenTables(src string) string {
	lines := strings.Split(src, "\n")
	var out []string
	skipNext := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if skipNext {
			skipNext = false
			continue
		}
		if isTableRow(line) && i+1 < len(lines) && isTableSeparator(lines[i+1]) {
			out = append(out, strings.Join(tableCells(line), " | "))
			// Skip the separator row; a following data row becomes the next line.
			skipNext = true
			continue
		}
		if isTableRow(line) {
			out = append(out, strings.Join(tableCells(line), " | "))
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func isTableRow(line string) bool {
	return strings.Contains(line, "|") && strings.TrimSpace(line) != ""
}

func isTableSeparator(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") && !strings.HasSuffix(trimmed, "|") {
		return false
	}
	noPipes := strings.ReplaceAll(trimmed, "|", "")
	if noPipes == "" {
		return true
	}
	return regexp.MustCompile(`^[\s\-:]+$`).MatchString(noPipes)
}

func tableCells(row string) []string {
	trimmed := strings.TrimSpace(row)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	parts := strings.Split(trimmed, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}
