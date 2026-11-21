// Command cairn is the shell and server for Cairn databases.
//
//	cairn [file]                 interactive SQL shell (in-memory without a file)
//	cairn exec <file> <sql>      run statements and print results
//	cairn check <file>           verify the database
//	cairn serve [-addr a] <file> web console and JSON API
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/integral0909/cairn"
)

const usage = `usage:
  cairn [file]                       open the SQL shell (in memory without a file)
  cairn exec [-csv] <file> <sql>     run statements and print the results
  cairn check <file>                 verify tables, indexes and free pages
  cairn serve [-addr host:port] <file>
                                     web console at http://127.0.0.1:8080`

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "exec":
			os.Exit(runExec(args[1:]))
		case "check":
			os.Exit(runCheck(args[1:]))
		case "serve":
			os.Exit(runServe(args[1:]))
		case "-h", "--help", "help":
			fmt.Println(usage)
			return
		}
		if strings.HasPrefix(args[0], "-") {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
	}
	path := cairn.Memory
	if len(args) > 0 {
		path = args[0]
	}
	db, err := cairn.Open(path, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairn:", err)
		os.Exit(1)
	}
	defer db.Close()
	sh := newShell(db, os.Stdin, os.Stdout, isTerminal(os.Stdin))
	sh.run()
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func runExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	csv := fs.Bool("csv", false, "print CSV instead of a table")
	fs.Parse(args)
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: cairn exec [-csv] <file> <sql>")
		return 2
	}
	db, err := cairn.Open(fs.Arg(0), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairn:", err)
		return 1
	}
	defer db.Close()
	sh := newShell(db, strings.NewReader(""), os.Stdout, false)
	if *csv {
		sh.mode = "csv"
	}
	if !sh.runSQL(fs.Arg(1)) {
		return 1
	}
	return 0
}

func runCheck(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: cairn check <file>")
		return 2
	}
	db, err := cairn.Open(args[0], nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairn:", err)
		return 1
	}
	defer db.Close()
	problems, err := db.Check()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairn:", err)
		return 1
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	st := db.Stats()
	if len(problems) > 0 {
		fmt.Printf("%d problems found\n", len(problems))
		return 1
	}
	fmt.Printf("ok: %d tables, %d pages\n", st.Tables, st.Pages)
	return 0
}
