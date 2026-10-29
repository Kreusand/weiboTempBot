package main

import (
	"html"
	"regexp"
	"strings"
)

var (
	// Elements whose content is never part of the visible text.
	hiddenTags = map[string]bool{"script": true, "style": true, "iframe": true, "video": true, "source": true}
	// Elements that end a line.
	blockTags = map[string]bool{"p": true, "div": true, "li": true, "tr": true, "br": true}

	horizontalSpace = regexp.MustCompile(`[ \t\x{00a0}\x{3000}]+`)
)

func asciiLower(text string) string {
	buffer := []byte(text)
	for index, char := range buffer {
		if char >= 'A' && char <= 'Z' {
			buffer[index] = char + ('a' - 'A')
		}
	}
	return string(buffer)
}

// parseTag returns the lower case element name of the text between "<" and ">" and whether it is a closing tag.
func parseTag(tag string) (name string, closing bool) {
	tag = strings.TrimSpace(tag)
	if strings.HasPrefix(tag, "/") {
		closing = true
		tag = tag[1:]
	}
	end := strings.IndexAny(tag, " \t\r\n/")
	if end >= 0 {
		tag = tag[:end]
	}
	return asciiLower(tag), closing
}

// cleanHTML turns the HTML fragments of Weibo (post text, comments) into plain text.
// Line breaks are kept, markup and hidden elements are dropped, entities are decoded.
func cleanHTML(value string) string {
	var out strings.Builder
	position := 0
	for position < len(value) {
		if value[position] != '<' {
			next := strings.IndexByte(value[position:], '<')
			end := len(value)
			if next >= 0 {
				end = position + next
			}
			out.WriteString(html.UnescapeString(value[position:end]))
			position = end
			continue
		}
		closeIndex := strings.IndexByte(value[position:], '>')
		if closeIndex < 0 {
			out.WriteString(html.UnescapeString(value[position:]))
			break
		}
		name, closing := parseTag(value[position+1 : position+closeIndex])
		position += closeIndex + 1
		switch {
		case blockTags[name]:
			out.WriteByte('\n')
		case hiddenTags[name] && !closing:
			rest := asciiLower(value[position:])
			endTag := strings.Index(rest, "</"+name)
			if endTag < 0 {
				position = len(value)
				break
			}
			tagEnd := strings.IndexByte(rest[endTag:], '>')
			if tagEnd < 0 {
				position = len(value)
				break
			}
			position += endTag + tagEnd + 1
		}
	}
	return normalizeText(out.String())
}

// normalizeText collapses runs of spaces, trims every line and keeps at most one blank line in a row.
func normalizeText(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var lines []string
	blank := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(horizontalSpace.ReplaceAllString(line, " "))
		if line == "" {
			blank = len(lines) > 0
			continue
		}
		if blank {
			lines = append(lines, "")
			blank = false
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// textOf cleans any value that may hold HTML.
func textOf(value any) string {
	return cleanHTML(asStr(value))
}
