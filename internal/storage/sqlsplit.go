package storage

import (
	"strings"
)

// splitSQLStatements preserves semicolons inside SQLite trigger bodies.
func splitSQLStatements(script string) []string {
	var statements []string
	start := 0
	blockDepth := 0
	quote := byte(0)
	bracketIdentifier := false
	lineComment := false
	blockComment := false

	for i := 0; i < len(script); {
		current := script[i]
		if lineComment {
			if current == '\n' {
				lineComment = false
			}
			i++
			continue
		}
		if blockComment {
			if current == '*' && i+1 < len(script) && script[i+1] == '/' {
				blockComment = false
				i += 2
				continue
			}
			i++
			continue
		}
		if quote != 0 {
			if current == quote {
				if i+1 < len(script) && script[i+1] == quote {
					i += 2
					continue
				}
				quote = 0
			}
			i++
			continue
		}
		if bracketIdentifier {
			if current == ']' {
				bracketIdentifier = false
			}
			i++
			continue
		}
		if current == '-' && i+1 < len(script) && script[i+1] == '-' {
			lineComment = true
			i += 2
			continue
		}
		if current == '/' && i+1 < len(script) && script[i+1] == '*' {
			blockComment = true
			i += 2
			continue
		}
		switch current {
		case '\'', '"', '`':
			quote = current
			i++
			continue
		case '[':
			bracketIdentifier = true
			i++
			continue
		case ';':
			if blockDepth == 0 {
				if statement := strings.TrimSpace(script[start:i]); statement != "" {
					statements = append(statements, statement)
				}
				start = i + 1
			}
			i++
			continue
		}
		if isSQLWordByte(current) {
			wordStart := i
			for i < len(script) && isSQLWordByte(script[i]) {
				i++
			}
			switch strings.ToUpper(script[wordStart:i]) {
			case "BEGIN":
				blockDepth++
			case "END":
				if blockDepth > 0 {
					blockDepth--
				}
			}
			continue
		}
		i++
	}
	if statement := strings.TrimSpace(script[start:]); statement != "" {
		statements = append(statements, statement)
	}
	return statements
}

func isSQLWordByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}
