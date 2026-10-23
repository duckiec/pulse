package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"pulse/internal/engine"
)

// runMerge is the `pulse merge <ancestor> <current> <other>` entry, shaped for
// Git's merge driver contract:
//
//	git config merge.pulse.driver "pulse merge %O %A %B"
//
// Git invokes the driver only after a textual conflict and expects the driver
// to write the (possibly conflicted) result to %A. Exit 0 means "resolved",
// exit 1 means "%A still contains conflicts" — Git stages it as conflicted and
// keeps both sides in the usual way.
//
// Exit codes: 0 clean, 1 unresolved conflicts, 2 usage or input error.
func runMerge(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "help" {
		printHelp(stdout, mergeHelp)
		return exitOK
	}
	if wantsFlagHelp(args) {
		printHelp(stdout, mergeHelp)
		return exitOK
	}
	if len(args) != 3 {
		fmt.Fprintln(stderr, "pulse merge: three files are required: ancestor current other")
		fmt.Fprintln(stderr, "bind it with: git config merge.pulse.driver \"pulse merge %O %A %B\"")
		return exitUsage
	}
	ancestor, current, other := args[0], args[1], args[2]

	baseSrc, err := os.ReadFile(ancestor)
	if err != nil {
		fmt.Fprintf(stderr, "pulse merge: read ancestor: %v\n", err)
		return exitUsage
	}
	oursSrc, err := os.ReadFile(current)
	if err != nil {
		fmt.Fprintf(stderr, "pulse merge: read current: %v\n", err)
		return exitUsage
	}
	theirsSrc, err := os.ReadFile(other)
	if err != nil {
		fmt.Fprintf(stderr, "pulse merge: read other: %v\n", err)
		return exitUsage
	}

	res, err := engine.Merge(baseSrc, oursSrc, theirsSrc)
	if err != nil {
		fmt.Fprintf(stderr, "pulse merge: %v\n", err)
		return exitUsage
	}

	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(current); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := writeMergeResult(current, res.Text, mode); err != nil {
		fmt.Fprintf(stderr, "pulse merge: %v\n", err)
		return exitUsage
	}

	if res.Conflicts > 0 {
		fmt.Fprintf(stdout, "merge: %d unresolved conflict(s) in %d request(s); resolve the markers in the current file\n",
			res.Conflicts, res.Requests)
		return exitFail
	}
	fmt.Fprintf(stdout, "merge: clean semantic merge (%d request(s))\n", res.Requests)
	return exitOK
}

// writeMergeResult replaces path atomically: a temp file in the same directory,
// fsync, rename. A merge driver that truncates %A and then dies leaves the user
// with neither side, which is the one failure mode worth the extra syscalls.
func writeMergeResult(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".pulse-merge-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
