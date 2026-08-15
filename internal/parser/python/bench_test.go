package python

import (
	"context"
	"strings"
	"testing"
)

func BenchmarkParseMethodHeavy(b *testing.B) {
	var sb strings.Builder
	for c := 0; c < 200; c++ {
		sb.WriteString("class C")
		sb.WriteString(strings.Repeat("x", c%5))
		sb.WriteString(":\n")
		for m := 0; m < 10; m++ {
			sb.WriteString("    def method_")
			sb.WriteString(strings.Repeat("m", m+1))
			sb.WriteString("(self):\n")
			sb.WriteString("        helper_one()\n        helper_two(value)\n        return helper_three()\n\n")
		}
	}
	src := []byte(sb.String())
	b.SetBytes(int64(len(src)))
	adapter := New()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := adapter.Parse(context.Background(), "bench.py", src); err != nil {
			b.Fatal(err)
		}
	}
}
