package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCommandNoWebHelpVersionAndMissingFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{nil, 0}, {[]string{"--help"}, 0}, {[]string{"--version"}, 0},
		{[]string{"--web"}, 2}, {[]string{"--web", "--workspace", "."}, 2},
		{[]string{"--unknown"}, 2}, {[]string{"unexpected"}, 2},
	} {
		var out, err bytes.Buffer
		if got := run(context.Background(), tc.args, &out, &err); got != tc.code {
			t.Errorf("exit=%d want=%d", got, tc.code)
		}
	}
}

func TestCommandSubprocessNoWebDoesNotListen(t *testing.T) {
	if os.Getenv("SEASPRAK_WEB_ENTRY_TEST") == "1" {
		os.Exit(run(context.Background(), []string{"--listen", os.Getenv("SEASPRAK_WEB_ENTRY_ADDRESS")}, os.Stdout, os.Stderr))
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listen failed")
	}
	defer ln.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCommandSubprocessNoWebDoesNotListen$")
	cmd.Env = append(os.Environ(), "SEASPRAK_WEB_ENTRY_TEST=1", "SEASPRAK_WEB_ENTRY_ADDRESS="+ln.Addr().String())
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal("no-web subprocess failed")
	}
	if !strings.Contains(string(b), "--web") {
		t.Error("missing usage")
	}
}
