// SPDX-License-Identifier: AGPL-3.0-only

package database

import "bytes"

// clickHouseFileLogsUnbounded reads the top-level logger's scalar settings
// from ClickHouse's preprocessed config. Only these five settings affect the
// warning, and the caller caps the input.
func clickHouseFileLogsUnbounded(config []byte) bool {
	fields := map[string]string{"level": "", "log": "", "errorlog": "", "size": "", "count": ""}
	const trackedDepth = 3
	path := make([][]byte, 0, trackedDepth)
	var depth int
	var logger bool
	var field string
	var value []byte
	for len(config) > 0 {
		body, rest, ok := bytes.Cut(config, []byte("<"))
		if depth == 3 && field != "" {
			value = append(value, body...)
		}
		if !ok {
			break
		}
		var marker string
		switch {
		case bytes.HasPrefix(rest, []byte("!--")):
			marker = "-->"
		case bytes.HasPrefix(rest, []byte("?")):
			marker = "?>"
		case bytes.HasPrefix(rest, []byte("![CDATA[")):
			marker = "]]>"
		}
		if marker != "" {
			body, config, ok = bytes.Cut(rest, []byte(marker))
			if !ok {
				return false
			}
			if marker == "]]>" && depth == 3 && field != "" {
				value = append(value, body[len("![CDATA["):]...)
			}
			continue
		}
		end := clickHouseTagEnd(rest)
		if end < 0 {
			return false
		}
		tag := bytes.TrimSpace(rest[:end])
		config = rest[end+1:]
		if bytes.HasPrefix(tag, []byte("/")) {
			if depth == 0 {
				return false
			}
			if depth <= trackedDepth {
				last := len(path) - 1
				if !bytes.Equal(tag[1:], path[last]) {
					return false
				}
				path = path[:last]
			}
			if depth == 3 && field != "" {
				fields[field], field = string(value), ""
			}
			if depth == 2 {
				logger = false
			}
			depth--
			if depth == 0 {
				break
			}
			continue
		}
		name := tag
		if i := bytes.IndexAny(tag, " \t\r\n/"); i >= 0 {
			name = tag[:i]
		}
		if depth == 1 {
			logger = bytes.Equal(name, []byte("logger"))
		}
		selfClosing := bytes.HasSuffix(tag, []byte("/"))
		if depth == 2 && logger {
			if _, wanted := fields[string(name)]; wanted {
				if selfClosing {
					fields[string(name)] = ""
				} else {
					field, value = string(name), value[:0]
				}
			}
		}
		if !selfClosing {
			if depth < trackedDepth {
				path = append(path, name)
			}
			depth++
		}
	}
	if depth != 0 || fields["log"] == "" && fields["errorlog"] == "" {
		return false
	}
	level := fields["level"]
	return level == "trace" || level == "debug" || level == "test" || fields["size"] == "" || fields["count"] == ""
}

// clickHouseTagEnd finds the close of a tag, skipping quoted attribute values.
func clickHouseTagEnd(tag []byte) int {
	for offset := 0; offset < len(tag); {
		i := bytes.IndexAny(tag[offset:], `>"'`)
		if i < 0 {
			return -1
		}
		i += offset
		if tag[i] == '>' {
			return i
		}
		closingQuote := bytes.IndexByte(tag[i+1:], tag[i])
		if closingQuote < 0 {
			return -1
		}
		offset = i + closingQuote + 2
	}
	return -1
}
