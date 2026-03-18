package texttoken

import "strings"

func Weights(content []byte) map[string]float64 {
	return WeightsString(string(content))
}

func WeightsString(text string) map[string]float64 {
	text = strings.ToLower(text)
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r == '/' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	out := map[string]float64{}
	for _, field := range fields {
		if len(field) < 2 {
			continue
		}
		out[field] += 1
	}
	return out
}
