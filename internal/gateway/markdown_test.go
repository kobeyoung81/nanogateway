package gateway

import "testing"

func TestMarkdownToText(t *testing.T) {
	input := `# Heading

This has **bold**, *italic*, and ` + "`code`" + `.

- item one
- item two

| Col A | Col B |
|-------|-------|
| 1     | 2     |

[link text](https://example.com) and ![alt](img.png)

> quote line

\` + "```" + `go
fmt.Println("hi")
\` + "```" + ``
	wantParts := []string{
		"Heading",
		"This has bold, italic, and code.",
		"• item one",
		"• item two",
		"Col A | Col B",
		"1 | 2",
		"link text (https://example.com) and alt",
		"quote line",
		`fmt.Println("hi")`,
	}
	got := MarkdownToText(input)
	for _, part := range wantParts {
		if !contains(got, part) {
			t.Errorf("MarkdownToText output missing %q\n---got---\n%s", part, got)
		}
	}
	if contains(got, "**bold**") {
		t.Errorf("bold markers were not stripped:\n%s", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(needle) == 0 || containsIndex(haystack, needle) >= 0)
}

func containsIndex(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
