package pipeline

import (
	"regexp"
	"strings"
)

var headerPattern = regexp.MustCompile(`(?m)^(##\s+.+)$`)

// Chunk represents a section of a markdown document.
type TextChunk struct {
	Heading string
	Content string
}

// EstimateTokens gives a rough token count (~4 chars per token).
func EstimateTokens(text string) int {
	return len(text) / 4
}

// ChunkMarkdown splits a markdown document on ## headers.
// Oversized chunks are further split on paragraph boundaries (\n\n).
// If no chunks are produced, the full text is returned as a single chunk.
func ChunkMarkdown(text string, maxTokens, minTokens int) []TextChunk {
	// Split on ## headers, keeping the headers as separators
	parts := headerPattern.Split(text, -1)
	headers := headerPattern.FindAllString(text, -1)

	var raw []TextChunk
	currentHeading := "(preamble)"
	currentText := ""

	for i, part := range parts {
		if i == 0 {
			// Text before first header
			currentText = part
		} else {
			// Save previous chunk if substantial
			if strings.TrimSpace(currentText) != "" && EstimateTokens(currentText) >= minTokens {
				raw = append(raw, TextChunk{Heading: currentHeading, Content: strings.TrimSpace(currentText)})
			}
			currentHeading = strings.TrimSpace(strings.TrimLeft(headers[i-1], "#"))
			currentText = part
		}
	}
	// Final chunk
	if strings.TrimSpace(currentText) != "" && EstimateTokens(currentText) >= minTokens {
		raw = append(raw, TextChunk{Heading: currentHeading, Content: strings.TrimSpace(currentText)})
	}

	// Split oversized chunks on paragraphs
	var result []TextChunk
	for _, chunk := range raw {
		if EstimateTokens(chunk.Content) <= maxTokens {
			result = append(result, chunk)
			continue
		}
		paragraphs := strings.Split(chunk.Content, "\n\n")
		current := ""
		for _, para := range paragraphs {
			if EstimateTokens(current+para) > maxTokens && current != "" {
				result = append(result, TextChunk{Heading: chunk.Heading, Content: strings.TrimSpace(current)})
				current = para + "\n\n"
			} else {
				current += para + "\n\n"
			}
		}
		if strings.TrimSpace(current) != "" && EstimateTokens(current) >= minTokens {
			result = append(result, TextChunk{Heading: chunk.Heading, Content: strings.TrimSpace(current)})
		}
	}

	// If no chunks produced, return entire text as single chunk
	if len(result) == 0 && strings.TrimSpace(text) != "" {
		result = append(result, TextChunk{Heading: "(full)", Content: strings.TrimSpace(text)})
	}

	return result
}

// NeedsChunking returns true for namespaces that should be split into chunks.
func NeedsChunking(namespace string) bool {
	switch namespace {
	case "learnings", "docs", "plans", "references":
		return true
	}
	return false
}

// NamespaceFromPath derives a namespace from a file path relative to the context dir.
func NamespaceFromPath(relPath string) string {
	prefixes := []string{
		"learnings/", "docs/", "plans/", "references/",
		"patterns/", "debugging/", "primitives/",
		"skills/internal/", "skills/external/",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(relPath, prefix) {
			return strings.TrimSuffix(prefix, "/")
		}
	}
	return "other"
}
