package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

const version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agentd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	printVersion := fs.Bool("version", false, "print version")
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			writeHelp(stdout)
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unknown argument: %s\n", fs.Arg(0))
		return 2
	}
	if *printVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	writeHelp(stdout)
	return 0
}

func writeHelp(w io.Writer) {
	fmt.Fprintln(w, "agentd is the Seasprak process entry.")
	fmt.Fprintln(w, "服务尚未实现")
	fmt.Fprintln(w, "Usage: agentd [--help] [--version]")
}
