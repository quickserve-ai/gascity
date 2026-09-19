package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Laurels v0 (ga-9obb1h): a seat may keep a short laurels.md holding praise about
// its work that a PERSON originated (the operator, a customer, a partner).
// SessionStart surfaces it with nothing attached: no task, no bead, no priority.
// It is read here, at hook time, and never rendered into the prompt template, so
// adding a laurel changes no prompt hash and never drifts or restarts a seat. An
// absent file injects nothing.
//
// The file lives in the seat's city-side home, <city>/.gc/agents/<GC_AGENT>, keyed
// by identity and never by work dir. A rig seat's GC_DIR is a repository checkout
// (often nested inside that same home), and any file in a checkout is repository
// content that this hook would label as recognition from a person. Two names are
// checked in order, seat/laurels.md then laurels.md. Every component below
// .gc/agents is opened relative to its parent with O_NOFOLLOW, so a symlink
// anywhere on the path is refused rather than followed out of the home. Angle
// brackets are escaped, so no laurel can close the wrapper or open a tag of its
// own. A laurel that exists but cannot be read is reported on stderr.
const (
	laurelsFileName = "laurels.md"
	// The spec bounds a seat's laurels at one paragraph; the cap keeps a file
	// that outgrew it from taxing every boot, and bounds the read itself.
	laurelsMaxBytes = 1200
)

var laurelsEscaper = strings.NewReplacer("<", "&lt;", ">", "&gt;")

func primeLaurelsInjection(cityPath, agent string, stderr io.Writer) string {
	parts := laurelsAgentPath(agent)
	if cityPath == "" || parts == nil {
		return ""
	}
	root := filepath.Join(cityPath, ".gc", "agents")
	for _, name := range [][]string{{"seat", laurelsFileName}, {laurelsFileName}} {
		rel := append(append([]string{}, parts...), name...)
		text, err := readLaurels(root, rel)
		if err != nil {
			fmt.Fprintf(stderr, "gc prime: laurels %s: %v\n", filepath.Join(append([]string{root}, rel...)...), err) //nolint:errcheck // best-effort hook diagnostics
			continue
		}
		if text != "" {
			return "\n\n<laurels>\nRecognition from people this seat has worked for. It carries no task, no bead and no priority; nothing here asks you to do anything.\n\n" +
				laurelsEscaper.Replace(text) + "\n</laurels>\n"
		}
	}
	return ""
}

// laurelsAgentPath splits a qualified agent name ("woodhouse", "qcore/archer")
// into path components, or returns nil when any component is empty, "." or "..".
func laurelsAgentPath(agent string) []string {
	if agent == "" {
		return nil
	}
	parts := strings.Split(agent, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil
		}
	}
	return parts
}

// readLaurels returns the trimmed, capped text of root/rel. An absent file is
// ("", nil); anything else that stops the read (a symlink on the path, a
// non-regular file, an I/O error) is an error for the caller to report. It
// reads at most laurelsMaxBytes+1 bytes.
func readLaurels(root string, rel []string) (string, error) {
	f, err := openNoFollow(root, rel)
	if errors.Is(err, unix.ENOENT) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	data, err := io.ReadAll(io.LimitReader(f, laurelsMaxBytes+1))
	if err != nil {
		return "", err
	}
	truncated := false
	if len(data) > laurelsMaxBytes {
		// Cut the raw bytes, not the trimmed text: data holds laurelsMaxBytes+1
		// bytes here, so data[cut] is always in range.
		cut := laurelsMaxBytes
		for cut > 0 && !utf8.RuneStart(data[cut]) {
			cut--
		}
		truncated = info.Size() > int64(laurelsMaxBytes+1) || strings.TrimSpace(string(data[cut:])) != ""
		data = data[:cut]
	}
	text := strings.TrimSpace(string(data))
	if text != "" && truncated {
		text += " [truncated]"
	}
	return text, nil
}

// openNoFollow opens root, then each component of rel relative to its parent,
// all with O_NOFOLLOW: directories with O_DIRECTORY, the file non-blocking so a
// FIFO cannot block gc prime. No component, root included, may be a symlink.
func openNoFollow(root string, rel []string) (*os.File, error) {
	dfd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	for i, name := range rel {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if i < len(rel)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			flags |= unix.O_NONBLOCK
		}
		fd, err := unix.Openat(dfd, name, flags, 0)
		_ = unix.Close(dfd)
		if err != nil {
			return nil, err
		}
		dfd = fd
	}
	return os.NewFile(uintptr(dfd), filepath.Join(append([]string{root}, rel...)...)), nil
}
