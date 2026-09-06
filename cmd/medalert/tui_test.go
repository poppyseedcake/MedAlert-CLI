package main_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestTerminalAccountKeyboardSmoke(t *testing.T) {
	fake, closeServer := newAuthFake(t)
	defer closeServer()
	root := privateTempDir(t)
	database := filepath.Join(root, "medalert.db")
	command := exec.Command(executablePath, "--database", database)
	command.Env = append(os.Environ(), "TERM=xterm-256color", "MEDALERT_NON_INTERACTIVE=", "MEDALERT_OUTPUT=text", "MEDALERT_SESSION_DIR="+filepath.Join(root, "sessions"), "MEDALERT_MEDICOVER_BASE_URL="+fake.baseURL)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer func() { _ = command.Process.Kill() }()
	var mu sync.Mutex
	var output bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := terminal.Read(buf)
			mu.Lock()
			output.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	cursor := 0
	await := func(text string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			current := output.String()
			mu.Unlock()
			if at := strings.Index(current[cursor:], text); at >= 0 {
				cursor += at + len(text)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		current := output.String()
		mu.Unlock()
		t.Fatalf("terminal did not show %q: %s", text, current)
	}
	send := func(keys string) {
		t.Helper()
		if _, err := terminal.Write([]byte(keys)); err != nil {
			t.Fatal(err)
		}
	}
	await("Stan systemu")
	await("Stan odświeżony.")
	send("2")
	await("Konta Medicover")
	send("a")
	await("Dodaj konto")
	send("home\tmfa-user@example.com\t\t\r")
	await("Zapisano konto.")
	send("l")
	await("Podaj hasło")
	send("mfa-pass\r")
	await("Podaj kod MFA")
	send("123456\r")
	await("Zalogowano konto.")
	send("o")
	await("Wylogować konto")
	send("\t\r")
	await("Wylogowano konto.")
	send("l")
	await("Podaj hasło")
	const marker = "TUI_SECRET_MARKER_32"
	send(marker)
	send("\x1b")
	await("Anulowano logowanie.")
	send("?")
	await("Pomoc")
	send("\x1b")
	if err := pty.Setsize(terminal, &pty.Winsize{Cols: 79, Rows: 23}); err != nil {
		t.Fatal(err)
	}
	_ = command.Process.Signal(unix.SIGWINCH)
	await("80 kolumn i 24 wiersze")
	if err := pty.Setsize(terminal, &pty.Winsize{Cols: 120, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	_ = command.Process.Signal(unix.SIGWINCH)
	await("Do zrobienia")
	send("q")
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal did not exit")
	}
	<-done
	mu.Lock()
	transcript := output.String()
	mu.Unlock()
	for _, secret := range []string{marker, "mfa-pass", "123456", "trusted-1", "mfa-t"} {
		if strings.Contains(transcript, secret) {
			t.Fatalf("terminal exposed secret %q", secret)
		}
	}
	for _, escape := range []string{"\x1b[?1049h", "\x1b[?1049l"} {
		if !strings.Contains(transcript, escape) {
			t.Fatalf("missing alternate screen sequence %q", escape)
		}
	}
	result := run(t, nil, "account", "show", "home", "--database", database, "--output", "json")
	if result.exitCode != 0 || !strings.Contains(result.stdout, "mfa-user@example.com") {
		t.Fatalf("saved account: %+v", result)
	}
}

func TestNoCommandWithoutTerminalReturnsUsageError(t *testing.T) {
	for _, args := range [][]string{nil, {"--non-interactive"}, {"--output", "json"}} {
		result := run(t, []string{"MEDALERT_NON_INTERACTIVE=", "MEDALERT_OUTPUT=text"}, args...)
		if result.exitCode != 2 || result.stdout != "" || strings.Contains(result.stderr, "\x1b[") {
			t.Fatalf("args %v: %+v", args, result)
		}
	}
}
