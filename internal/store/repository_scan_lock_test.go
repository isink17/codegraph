package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRepositoryScanLockIsolationAndRelease(t *testing.T) {
	s, _ := newQueryTestStore(t)
	other := openSameStore(t, s)
	ctx := context.Background()
	root := t.TempDir()
	release, err := s.AcquireRepositoryScan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if unlock, err := other.AcquireRepositoryScan(ctx, filepath.Join(root, ".")); !errors.Is(err, ErrScanActive) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("same repository admitted: %v", err)
	}
	independent, err := other.AcquireRepositoryScan(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	independent()
	release()
	again, err := other.AcquireRepositoryScan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	again()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.AcquireRepositoryScan(cancelled, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission: %v", err)
	}
}

func TestRepositoryScanLockProcessExit(t *testing.T) {
	if path := os.Getenv("CODEGRAPH_TEST_WRITER_DB"); path != "" {
		s := &Store{path: path}
		release, err := s.AcquireRepositoryScan(context.Background(), os.Getenv("CODEGRAPH_TEST_WRITER_ROOT"))
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		fmt.Println("writer ready")
		var line string
		_, _ = fmt.Fscanln(os.Stdin, &line)
		return
	}
	s, _ := newQueryTestStore(t)
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRepositoryScanLockProcessExit$")
	cmd.Env = append(os.Environ(), "CODEGRAPH_TEST_WRITER_DB="+s.path, "CODEGRAPH_TEST_WRITER_ROOT="+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "writer ready\n" {
		t.Fatalf("child ready=%q err=%v", line, err)
	}
	if unlock, err := s.AcquireRepositoryScan(context.Background(), root); !errors.Is(err, ErrScanActive) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("child lock ignored: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	release, err := s.AcquireRepositoryScan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
