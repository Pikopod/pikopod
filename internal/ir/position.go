package ir

type Position struct {
	File string `json:"file,omitempty"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

type Positions map[string]Position

func (p Positions) Lookup(pointer string) (Position, bool) {
	if p == nil || pointer == "" {
		return Position{}, false
	}
	for ptr := pointer; ptr != "#"; {
		if pos, ok := p[ptr]; ok {
			return pos, true
		}
		i := lastSlash(ptr)
		if i <= 0 {
			return Position{}, false
		}
		ptr = ptr[:i]
	}
	return Position{}, false
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
