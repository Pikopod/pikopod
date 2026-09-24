package importer

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type boundedJSONParser struct {
	s      string
	i      int
	nodes  int
	limits ParseLimits
}

func parseJSONSafely(text string, limits ParseLimits) (any, error) {
	if len(text) > limits.MaxDocumentBytes {
		return nil, specErr(SpecTooLarge, "document exceeds the size limit")
	}
	p := &boundedJSONParser{s: text, limits: limits}
	return p.parse()
}

func (p *boundedJSONParser) parse() (any, error) {
	p.skipWs()
	value, err := p.parseValue(0)
	if err != nil {
		return nil, err
	}
	p.skipWs()
	if p.i != len(p.s) {
		return nil, p.fail("unexpected trailing content")
	}
	return value, nil
}

func (p *boundedJSONParser) parseValue(depth int) (any, error) {
	if depth > p.limits.MaxDepth {
		return nil, specErr(SpecDepthExceeded, fmt.Sprintf("nesting exceeds %d", p.limits.MaxDepth))
	}
	p.nodes++
	if p.nodes > p.limits.MaxNodes {
		return nil, specErr(SpecTooLarge, fmt.Sprintf("document exceeds %d nodes", p.limits.MaxNodes))
	}
	p.skipWs()
	if p.i >= len(p.s) {
		return nil, p.fail("unexpected end of input")
	}
	c := p.s[p.i]
	switch {
	case c == '{':
		return p.parseObject(depth)
	case c == '[':
		return p.parseArray(depth)
	case c == '"':
		return p.parseString()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	case strings.HasPrefix(p.s[p.i:], "true"):
		p.i += 4
		return true, nil
	case strings.HasPrefix(p.s[p.i:], "false"):
		p.i += 5
		return false, nil
	case strings.HasPrefix(p.s[p.i:], "null"):
		p.i += 4
		return nil, nil
	default:
		return nil, p.fail(fmt.Sprintf("unexpected token '%c'", c))
	}
}

func (p *boundedJSONParser) parseObject(depth int) (any, error) {
	p.i++
	out := NewOrdMap()
	p.skipWs()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return out, nil
	}
	for {
		p.skipWs()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, p.fail("expected string key")
		}
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		if out.Has(key) {
			return nil, specErr(SpecParseError, fmt.Sprintf("duplicate object key %q", key))
		}
		p.skipWs()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, p.fail(`expected ":"`)
		}
		p.i++
		value, err := p.parseValue(depth + 1)
		if err != nil {
			return nil, err
		}
		out.Set(key, value)
		p.skipWs()
		if p.i >= len(p.s) {
			return nil, p.fail(`expected "," or "}"`)
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return out, nil
		default:
			return nil, p.fail(`expected "," or "}"`)
		}
	}
}

func (p *boundedJSONParser) parseArray(depth int) (any, error) {
	p.i++
	out := []any{}
	p.skipWs()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return out, nil
	}
	for {
		value, err := p.parseValue(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
		p.skipWs()
		if p.i >= len(p.s) {
			return nil, p.fail(`expected "," or "]"`)
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return out, nil
		default:
			return nil, p.fail(`expected "," or "]"`)
		}
	}
}

func (p *boundedJSONParser) parseString() (string, error) {
	p.i++
	var b strings.Builder

	var pendingHigh rune = -1
	flushPending := func() {
		if pendingHigh >= 0 {
			b.WriteRune(utf8.RuneError)
			pendingHigh = -1
		}
	}
	for {
		if p.i >= len(p.s) {
			return "", failErr(p, "unterminated string")
		}
		c := p.s[p.i]
		p.i++
		if c == '"' {
			flushPending()
			return b.String(), nil
		}
		if c == '\\' {
			if p.i >= len(p.s) {
				return "", failErr(p, "invalid escape sequence")
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case '"', '\\', '/':
				flushPending()
				b.WriteByte(e)
			case 'b':
				flushPending()
				b.WriteByte('\b')
			case 'f':
				flushPending()
				b.WriteByte('\f')
			case 'n':
				flushPending()
				b.WriteByte('\n')
			case 'r':
				flushPending()
				b.WriteByte('\r')
			case 't':
				flushPending()
				b.WriteByte('\t')
			case 'u':
				if p.i+4 > len(p.s) || !isHex4(p.s[p.i:p.i+4]) {
					return "", failErr(p, "invalid unicode escape")
				}
				n, _ := strconv.ParseUint(p.s[p.i:p.i+4], 16, 32)
				p.i += 4
				cu := rune(n)
				if pendingHigh >= 0 {
					if utf16.IsSurrogate(cu) && cu >= 0xDC00 {
						b.WriteRune(utf16.DecodeRune(pendingHigh, cu))
						pendingHigh = -1
						continue
					}
					flushPending()
				}
				if utf16.IsSurrogate(cu) {
					if cu < 0xDC00 {
						pendingHigh = cu
					} else {
						b.WriteRune(utf8.RuneError)
					}
				} else {
					b.WriteRune(cu)
				}
			default:
				return "", failErr(p, "invalid escape sequence")
			}
		} else if c < 0x20 {
			return "", failErr(p, "control character in string")
		} else {
			flushPending()
			b.WriteByte(c)
		}
	}
}

func (p *boundedJSONParser) parseNumber() (any, error) {
	start := p.i
	if p.i < len(p.s) && p.s[p.i] == '-' {
		p.i++
	}
	if p.i < len(p.s) && p.s[p.i] == '0' {
		p.i++
	} else if p.isDigit() {
		for p.isDigit() {
			p.i++
		}
	} else {
		return nil, p.fail("invalid number")
	}
	if p.i < len(p.s) && p.s[p.i] == '.' {
		p.i++
		if !p.isDigit() {
			return nil, p.fail("invalid number (fraction)")
		}
		for p.isDigit() {
			p.i++
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if !p.isDigit() {
			return nil, p.fail("invalid number (exponent)")
		}
		for p.isDigit() {
			p.i++
		}
	}
	f, err := strconv.ParseFloat(p.s[start:p.i], 64)
	if err != nil {
		return nil, p.fail("invalid number")
	}
	return f, nil
}

func (p *boundedJSONParser) isDigit() bool {
	return p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9'
}

func (p *boundedJSONParser) skipWs() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *boundedJSONParser) fail(message string) error {
	return specErr(SpecParseError, fmt.Sprintf("%s at position %d", message, p.i))
}

func failErr(p *boundedJSONParser, message string) error {
	return p.fail(message)
}

func isHex4(s string) bool {
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
