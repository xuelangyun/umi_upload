package umi

import (
	"strconv"
	"strings"
)

// ParseSimpleYAML parses configuration YAML files (datacollect_conf.yaml and metadata/conf.yaml)
// into map[string]any without external YAML dependencies.
// Supports YAML tags (!<tag:yaml.org,2002:str>, !!str), comments, single/double quotes,
// flow sequences [...], flow mappings {...}, block sequences (- item), and nested block mappings.
func ParseSimpleYAML(text string) map[string]any {
	out := map[string]any{}
	text = strings.TrimPrefix(text, "\uFEFF")
	rawLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	type lineInfo struct {
		indent  int
		trimmed string
	}
	var lines []lineInfo
	for _, rl := range rawLines {
		cleaned := stripYAMLComment(rl)
		trimmed := strings.TrimSpace(cleaned)
		if trimmed == "" || trimmed == "---" || trimmed == "..." {
			continue
		}
		indent := 0
		for _, ch := range cleaned {
			if ch == ' ' {
				indent++
			} else if ch == '\t' {
				indent += 2
			} else {
				break
			}
		}
		lines = append(lines, lineInfo{indent: indent, trimmed: trimmed})
	}

	for i := 0; i < len(lines); {
		ln := lines[i]
		if ln.indent != 0 {
			i++
			continue
		}
		if strings.HasPrefix(ln.trimmed, "- ") || ln.trimmed == "-" {
			// Top-level sequence, not a dictionary
			return map[string]any{}
		}
		colon := findYAMLColon(ln.trimmed)
		if colon <= 0 {
			i++
			continue
		}
		key := unquoteYAMLScalar(strings.TrimSpace(ln.trimmed[:colon]))
		rest := strings.TrimSpace(ln.trimmed[colon+1:])
		rest = stripYAMLTag(rest)

		if rest != "" {
			if strings.HasPrefix(rest, "{") {
				// Consume balanced braces if multi-line flow map
				combined := rest
				for !balancedDelims(combined, '{', '}') && i+1 < len(lines) {
					i++
					combined += " " + lines[i].trimmed
				}
				out[key] = parseYAMLFlowMap(combined)
			} else if strings.HasPrefix(rest, "[") {
				combined := rest
				for !balancedDelims(combined, '[', ']') && i+1 < len(lines) {
					i++
					combined += " " + lines[i].trimmed
				}
				out[key] = parseYAMLFlowSeq(combined)
			} else if rest == "|" || rest == ">" || rest == "|-" || rest == ">-" {
				var blk []string
				j := i + 1
				for j < len(lines) && lines[j].indent > 0 {
					blk = append(blk, lines[j].trimmed)
					j++
				}
				sep := "\n"
				if strings.HasPrefix(rest, ">") {
					sep = " "
				}
				out[key] = strings.Join(blk, sep)
				i = j
				continue
			} else {
				out[key] = parseYAMLScalar(rest)
			}
			i++
			continue
		}

		// Empty value on key line -> inspect indented children
		j := i + 1
		if j >= len(lines) || lines[j].indent == 0 {
			out[key] = nil
			i++
			continue
		}

		childIndent := lines[j].indent
		if strings.HasPrefix(lines[j].trimmed, "-") {
			var seq []any
			for j < len(lines) && lines[j].indent >= childIndent {
				if lines[j].indent == childIndent && (lines[j].trimmed == "-" || strings.HasPrefix(lines[j].trimmed, "- ")) {
					itemStr := strings.TrimSpace(strings.TrimPrefix(lines[j].trimmed, "-"))
					itemStr = stripYAMLTag(itemStr)
					if itemStr == "" {
						// Nested block under '-'
						k := j + 1
						sub := map[string]any{}
						for k < len(lines) && lines[k].indent > childIndent {
							if c := findYAMLColon(lines[k].trimmed); c > 0 {
								sk := unquoteYAMLScalar(strings.TrimSpace(lines[k].trimmed[:c]))
								sv := strings.TrimSpace(lines[k].trimmed[c+1:])
								sub[sk] = parseYAMLScalar(stripYAMLTag(sv))
							}
							k++
						}
						seq = append(seq, sub)
						j = k
						continue
					}
					if c := findYAMLColon(itemStr); c > 0 {
						// Inline map item `- k: v` plus indented continuation lines
						sub := map[string]any{}
						sk := unquoteYAMLScalar(strings.TrimSpace(itemStr[:c]))
						sv := strings.TrimSpace(itemStr[c+1:])
						sub[sk] = parseYAMLScalar(stripYAMLTag(sv))
						k := j + 1
						for k < len(lines) && lines[k].indent > childIndent {
							if c2 := findYAMLColon(lines[k].trimmed); c2 > 0 {
								sk2 := unquoteYAMLScalar(strings.TrimSpace(lines[k].trimmed[:c2]))
								sv2 := strings.TrimSpace(lines[k].trimmed[c2+1:])
								sub[sk2] = parseYAMLScalar(stripYAMLTag(sv2))
							}
							k++
						}
						seq = append(seq, sub)
						j = k
						continue
					}
					seq = append(seq, parseYAMLScalar(itemStr))
				}
				j++
			}
			out[key] = seq
			i = j
			continue
		}

		// Nested block mapping
		sub := map[string]any{}
		for j < len(lines) && lines[j].indent > 0 {
			if lines[j].indent == childIndent {
				if c := findYAMLColon(lines[j].trimmed); c > 0 {
					sk := unquoteYAMLScalar(strings.TrimSpace(lines[j].trimmed[:c]))
					sv := strings.TrimSpace(lines[j].trimmed[c+1:])
					sv = stripYAMLTag(sv)
					if sv != "" {
						sub[sk] = parseYAMLScalar(sv)
					} else {
						sub[sk] = nil
					}
				}
			}
			j++
		}
		out[key] = sub
		i = j
	}
	return out
}

func stripYAMLComment(line string) string {
	var inSingle, inDouble, escaped bool
	inTag := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble && ch == '\\' {
			escaped = true
			continue
		}
		if !inDouble && ch == '\'' {
			inSingle = !inSingle
			continue
		}
		if !inSingle && ch == '"' {
			inDouble = !inDouble
			continue
		}
		if !inSingle && !inDouble {
			if ch == '<' && i > 0 && line[i-1] == '!' {
				inTag = true
				continue
			}
			if inTag && ch == '>' {
				inTag = false
				continue
			}
			if !inTag && ch == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
				return line[:i]
			}
		}
	}
	return line
}

func findYAMLColon(s string) int {
	var inSingle, inDouble, escaped, inTag bool
	depth := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble && ch == '\\' {
			escaped = true
			continue
		}
		if !inDouble && ch == '\'' {
			inSingle = !inSingle
			continue
		}
		if !inSingle && ch == '"' {
			inDouble = !inDouble
			continue
		}
		if inSingle || inDouble {
			continue
		}
		if ch == '<' && i > 0 && s[i-1] == '!' {
			inTag = true
			continue
		}
		if inTag {
			if ch == '>' {
				inTag = false
			}
			continue
		}
		switch ch {
		case '{', '[':
			depth++
		case '}', ']':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 && (i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t') {
				return i
			}
		}
	}
	return -1
}

func stripYAMLTag(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "!<") {
		if idx := strings.IndexByte(s, '>'); idx != -1 {
			return strings.TrimSpace(s[idx+1:])
		}
	}
	if strings.HasPrefix(s, "!") && !strings.HasPrefix(s, "! ") {
		if idx := strings.IndexAny(s, " \t"); idx != -1 {
			return strings.TrimSpace(s[idx+1:])
		}
		return ""
	}
	return s
}

func balancedDelims(s string, openCh, closeCh byte) bool {
	var inSingle, inDouble, escaped bool
	depth := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble && ch == '\\' {
			escaped = true
			continue
		}
		if !inDouble && ch == '\'' {
			inSingle = !inSingle
			continue
		}
		if !inSingle && ch == '"' {
			inDouble = !inDouble
			continue
		}
		if inSingle || inDouble {
			continue
		}
		if ch == openCh {
			depth++
		} else if ch == closeCh {
			depth--
		}
	}
	return depth <= 0
}

func splitFlowItems(inner string) []string {
	var items []string
	var inSingle, inDouble, escaped, inTag bool
	depth := 0
	start := 0
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble && ch == '\\' {
			escaped = true
			continue
		}
		if !inDouble && ch == '\'' {
			inSingle = !inSingle
			continue
		}
		if !inSingle && ch == '"' {
			inDouble = !inDouble
			continue
		}
		if inSingle || inDouble {
			continue
		}
		if ch == '<' && i > 0 && inner[i-1] == '!' {
			inTag = true
			continue
		}
		if inTag {
			if ch == '>' {
				inTag = false
			}
			continue
		}
		switch ch {
		case '{', '[':
			depth++
		case '}', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				items = append(items, strings.TrimSpace(inner[start:i]))
				start = i + 1
			}
		}
	}
	if tail := strings.TrimSpace(inner[start:]); tail != "" {
		items = append(items, tail)
	}
	return items
}

func parseYAMLFlowSeq(s string) []any {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if s == "" {
		return []any{}
	}
	parts := splitFlowItems(s)
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		p = stripYAMLTag(p)
		if strings.HasPrefix(p, "{") {
			out = append(out, parseYAMLFlowMap(p))
		} else if strings.HasPrefix(p, "[") {
			out = append(out, parseYAMLFlowSeq(p))
		} else {
			out = append(out, parseYAMLScalar(p))
		}
	}
	return out
}

func parseYAMLFlowMap(s string) map[string]any {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	out := map[string]any{}
	if s == "" {
		return out
	}
	for _, p := range splitFlowItems(s) {
		c := findYAMLColon(p)
		if c <= 0 {
			continue
		}
		k := unquoteYAMLScalar(strings.TrimSpace(p[:c]))
		v := stripYAMLTag(strings.TrimSpace(p[c+1:]))
		if strings.HasPrefix(v, "{") {
			out[k] = parseYAMLFlowMap(v)
		} else if strings.HasPrefix(v, "[") {
			out[k] = parseYAMLFlowSeq(v)
		} else {
			out[k] = parseYAMLScalar(v)
		}
	}
	return out
}

func unquoteYAMLScalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s[1 : len(s)-1]
	}
	return s
}

func parseYAMLScalar(s string) any {
	s = strings.TrimSpace(s)
	if s == "" || s == "~" || s == "null" || s == "Null" || s == "NULL" {
		return nil
	}
	return unquoteYAMLScalar(s)
}
