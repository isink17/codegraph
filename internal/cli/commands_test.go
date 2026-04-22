package cli

import (
	"testing"
)

func TestRegisterCommandPanicsOnDuplicate(t *testing.T) {
	reg := map[string]*command{}
	registerCommand(reg, &command{name: "a"})
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on duplicate key, got nil")
		}
	}()
	registerCommand(reg, &command{name: "a"})
}

func TestRegisterCommandPanicsOnAliasDuplicate(t *testing.T) {
	reg := map[string]*command{}
	registerCommand(reg, &command{name: "a", aliases: []string{"x"}})
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on duplicate alias key, got nil")
		}
	}()
	registerCommand(reg, &command{name: "b", aliases: []string{"x"}})
}
