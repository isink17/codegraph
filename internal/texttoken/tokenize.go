package texttoken

// Tokenization rules (must match historical behavior):
// - case-insensitive
// - tokens include a-z, 0-9, and the characters: '_', '-', '.', '/'
// - any other rune/byte is a delimiter
// - tokens shorter than 2 characters are ignored

func Weights(content []byte) map[string]float64 {
	out := map[string]float64{}
	WeightsBytesInto(out, content)
	return out
}

func WeightsString(text string) map[string]float64 {
	out := map[string]float64{}
	WeightsStringInto(out, text)
	return out
}

func WeightsStrings(parts ...string) map[string]float64 {
	out := map[string]float64{}
	WeightsStringsInto(out, parts...)
	return out
}

func WeightsBytesInto(dst map[string]float64, content []byte) {
	if len(content) == 0 {
		return
	}
	var buf [128]byte
	token := buf[:0]
	flush := func() {
		if len(token) < 2 {
			token = token[:0]
			return
		}
		dst[string(token)] += 1
		token = token[:0]
	}

	for _, b := range content {
		if isTokenByte(b) {
			if b >= 'A' && b <= 'Z' {
				b = b + ('a' - 'A')
			}
			if len(token) < cap(token) {
				token = append(token, b)
			} else {
				// Rare slow path for very long tokens.
				token = append(token, b)
			}
			continue
		}
		if len(token) > 0 {
			flush()
		}
	}
	if len(token) > 0 {
		flush()
	}
}

func WeightsStringInto(dst map[string]float64, text string) {
	if text == "" {
		return
	}
	var buf [128]byte
	token := buf[:0]
	flush := func() {
		if len(token) < 2 {
			token = token[:0]
			return
		}
		dst[string(token)] += 1
		token = token[:0]
	}

	for i := 0; i < len(text); i++ {
		b := text[i]
		if isTokenByte(b) {
			if b >= 'A' && b <= 'Z' {
				b = b + ('a' - 'A')
			}
			token = append(token, b)
			continue
		}
		if len(token) > 0 {
			flush()
		}
	}
	if len(token) > 0 {
		flush()
	}
}

func WeightsStringsInto(dst map[string]float64, parts ...string) {
	for _, part := range parts {
		WeightsStringInto(dst, part)
	}
}

func isTokenByte(b byte) bool {
	switch {
	case b == '_' || b == '-' || b == '.' || b == '/':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	default:
		return false
	}
}
