package a

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("FIXTURE_HANG_MAIN") == "1" {
		time.Sleep(time.Hour)
	}
	os.Exit(m.Run())
}

func TestA(t *testing.T)      {}
func TestAB(t *testing.T)     {}
func TestÜber(t *testing.T)   {}
func Test_under(t *testing.T) {}

func TestSub(t *testing.T) {
	for _, n := range []string{"one", "two", "x/y", "with space"} {
		t.Run(n, func(t *testing.T) {})
	}
}

func TestSkip(t *testing.T) { t.Skip("always") }

func TestFail(t *testing.T) {
	if os.Getenv("FIXTURE_FAIL") == "1" {
		t.Fatal("asked to fail")
	}
}

func TestHang(t *testing.T) {
	if os.Getenv("FIXTURE_HANG") != "1" {
		return
	}
	if f := os.Getenv("FIXTURE_PIDFILE"); f != "" {
		_ = os.WriteFile(f, []byte(fmt.Sprint(os.Getpid())), 0o644)
	}
	time.Sleep(time.Hour)
}

func FuzzEcho(f *testing.F) {
	f.Add("seed")
	f.Fuzz(func(t *testing.T, s string) {})
}

func ExampleHello() {
	fmt.Println(Hello())
	// Output: hello
}

func BenchmarkHello(b *testing.B) {
	for range b.N {
		Hello()
	}
}
