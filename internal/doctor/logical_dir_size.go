package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// worktreeSizeBudget caps the time one worktree size check spends
	// sizing trees. Doctor abandons a check still running at its per-check
	// timeout (--check-timeout, default 1m) as "outcome unknown", which is
	// how worktree-disk-size gave no verdict at all on a 180 GB tree of 162
	// worktrees (ga-hyhccs). Sizing stops at this budget, or sooner when the
	// per-check timeout is lower (see sizingBudget), and whatever is
	// unfinished is reported as a labeled lower bound.
	worktreeSizeBudget = 40 * time.Second

	// dirMeasureConcurrency caps the du walks one check runs at once. Walks
	// run side by side so one huge tree cannot spend the time the others
	// need; the cap keeps a city with many rigs from flooding one disk.
	dirMeasureConcurrency = 4

	// duWaitDelay bounds how long a stopped du may take to exit and release
	// its output pipe before its result is taken as it stands.
	duWaitDelay = 2 * time.Second

	// duComplaintsLimit caps how much of du's stderr is kept for the error
	// message. Whether any line was a permission failure is tracked over all
	// of it.
	duComplaintsLimit = 4096
)

var (
	// errSizeWalkStuck marks a walk abandoned because it did not stop when its
	// deadline passed: a du that survives being killed is blocked in the
	// kernel, on a hung mount or a failing disk, not merely slow.
	errSizeWalkStuck = errors.New("size walk did not stop at its deadline and was abandoned")
	// errSizeWalkStarved marks a root whose walk never started because the
	// budget ran out while other roots held every measuring slot. Its tree
	// may be small.
	errSizeWalkStarved = errors.New("size walk never started: the budget ran out while other walks held every measuring slot")
)

// sizingBudget is the time a size check may spend measuring: own (zero
// means worktreeSizeBudget), cut to two thirds of the time left when the
// runner will abandon the check sooner, so the walks, their abandon grace and
// the verdict all land before ctx.Deadline. Rounded so the lower-bound label
// reads "within 20s", not "within 19.99987s".
func sizingBudget(ctx *CheckContext, own time.Duration) time.Duration {
	if own <= 0 {
		own = worktreeSizeBudget
	}
	if ctx == nil || ctx.Deadline.IsZero() {
		return own
	}
	if left := time.Until(ctx.Deadline) * 2 / 3; left < own {
		own = left
	}
	if own >= 2*time.Second {
		return own.Round(time.Second)
	}
	return own.Round(10 * time.Millisecond)
}

// dirSize is one directory tree's du-derived footprint.
//
// The count is LOGICAL: du charges every APFS clone at full size, so the space
// deleting the tree would reclaim can be far smaller (8-17x measured on
// worktrees provisioned with clonefile, ga-hyhccs). Print it only through
// logicalSizeLabel, never as a bare disk figure.
type dirSize struct {
	bytes  int64
	exists bool
	// lowerBound is true when the walk stopped at its budget: bytes then
	// counts only the parts of the tree sized before the deadline, and the
	// whole tree holds at least that much.
	lowerBound bool
}

// dirMeasure sizes the tree at root, stopping at ctx's deadline.
type dirMeasure func(ctx context.Context, root string) (dirSize, error)

// logicalSizeLabel renders a dirSize with the labels a reader needs: that the
// count is logical, and whether it is only a lower bound.
func logicalSizeLabel(s dirSize, budget time.Duration) string {
	if s.lowerBound {
		return fmt.Sprintf("at least %s logical (lower bound: the size walk did not finish within %s)", humanSize(s.bytes), budget)
	}
	return humanSize(s.bytes) + " logical"
}

// measuredDir is one root's outcome from measureDirsWithin.
type measuredDir struct {
	size dirSize
	err  error
}

// measureDirsWithin sizes each root side by side under one deadline, budget
// from now, and returns one result per root in order. A measurer still running
// an eighth of the budget past the deadline is abandoned and its root reported
// as not sized, so the call returns in bounded time even when a walk cannot be
// stopped (du blocked in the kernel on a hung mount).
func measureDirsWithin(budget time.Duration, measure dirMeasure, roots []string) []measuredDir {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	type indexed struct {
		i int
		m measuredDir
	}
	// Buffered so an abandoned measurer can still deliver and exit.
	results := make(chan indexed, len(roots))
	slots := make(chan struct{}, dirMeasureConcurrency)
	for i, root := range roots {
		go func() {
			var m measuredDir
			defer func() {
				// These goroutines run outside the doctor runner's panic
				// recovery: a panicking walk fails its root, not gc doctor.
				if p := recover(); p != nil {
					m = measuredDir{err: fmt.Errorf("size walk panicked: %v", p)}
				}
				results <- indexed{i, m}
			}()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				m.err = fmt.Errorf("%w (budget %s)", errSizeWalkStarved, budget)
				return
			}
			defer func() { <-slots }()
			if ctx.Err() != nil {
				m.err = fmt.Errorf("%w (budget %s)", errSizeWalkStarved, budget)
				return
			}
			m.size, m.err = measure(ctx, root)
		}()
	}

	out := make([]measuredDir, len(roots))
	received := make([]bool, len(roots))
	abandon := time.NewTimer(budget + budget/8)
	defer abandon.Stop()
	for range roots {
		select {
		case r := <-results:
			out[r.i] = r.m
			received[r.i] = true
		case <-abandon.C:
			for i, ok := range received {
				if !ok {
					out[i].err = fmt.Errorf("%w (budget %s)", errSizeWalkStuck, budget)
				}
			}
			return out
		}
	}
	return out
}

// duDirSizeWithin measures root with du, stopping at ctx's deadline. Unlike
// duDirBytes it keeps what it counted when time runs out: du -k prints each
// directory's total as it finishes that directory, so the finished subtrees
// seen before the deadline sum to a lower bound on the whole tree. A finished
// walk reports du's own total for root, exactly as du -sk would.
func duDirSizeWithin(ctx context.Context, root string) (dirSize, error) {
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return dirSize{}, nil
		}
		return dirSize{}, err
	}
	if !info.IsDir() {
		return dirSize{}, fmt.Errorf("%s is not a directory", root)
	}

	tally := duSubtreeTally{root: root}
	var complaints duComplaints
	stderr := &lineSplitter{onLine: complaints.add}
	cmd := exec.CommandContext(ctx, "du", "-k", root)
	cmd.Stdout = &lineSplitter{onLine: tally.add}
	cmd.Stderr = stderr
	cmd.WaitDelay = duWaitDelay
	err = cmd.Run()
	complaints.add(stderr.pending)

	switch {
	case err == nil:
		if !tally.sawRoot {
			return dirSize{}, fmt.Errorf("measure directory with du -k: no total printed for %s", root)
		}
		return dirSize{bytes: tally.rootKB * 1024, exists: true}, nil
	case errors.Is(err, exec.ErrNotFound):
		counted, exists, walkErr := countDirBytes(ctx, root)
		if walkErr != nil && ctx.Err() == nil {
			return dirSize{}, walkErr
		}
		return dirSize{bytes: counted, exists: exists, lowerBound: walkErr != nil}, nil
	case ctx.Err() != nil:
		if tally.garbled {
			return dirSize{}, fmt.Errorf("measure directory with du -k: walk did not finish, and its partial output cannot bound the size because a path name contains a newline: %w", ctx.Err())
		}
		return dirSize{bytes: tally.bytes(), exists: true, lowerBound: true}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return dirSize{}, duExitError("du -k", err, complaints.denied, complaints.text.String())
	}
	return dirSize{}, fmt.Errorf("measure directory with du -k: %w", err)
}

// lineSplitter is an io.Writer that hands each newline-terminated line to
// onLine, holding a partial line until the rest of it arrives.
type lineSplitter struct {
	pending []byte
	onLine  func(line []byte)
}

// Write implements io.Writer.
func (l *lineSplitter) Write(p []byte) (int, error) {
	l.pending = append(l.pending, p...)
	start := 0
	for {
		i := bytes.IndexByte(l.pending[start:], '\n')
		if i < 0 {
			break
		}
		l.onLine(l.pending[start : start+i])
		start += i + 1
	}
	l.pending = append(l.pending[:0], l.pending[start:]...)
	return len(p), nil
}

// duSubtreeTally keeps a running lower bound over du -k output for root. du
// prints a directory's total when it finishes the directory, after the totals
// of everything inside it, so the printed directories not inside another
// printed directory are disjoint finished subtrees; their sum is what du had
// fully counted. Those outermost subtrees are kept as a stack: a newly printed
// directory replaces the entries on top that lie inside it.
type duSubtreeTally struct {
	root     string
	finished []duSubtree
	sumKB    int64
	// rootKB is du's own total for root, from its last line.
	rootKB  int64
	sawRoot bool
	// garbled is set by any line that is not "<KiB>\t<root or a path under
	// it>". du prints names raw, so a name containing a newline splits into
	// a cut-off path and a stray fragment, and a cut-off path no longer
	// matches the directory that contains it: the stack can then count a
	// subtree twice, and its sum is no longer a lower bound.
	garbled bool
}

type duSubtree struct {
	path string
	kb   int64
}

// add records one "<KiB>\t<path>" line of du -k output.
func (t *duSubtreeTally) add(line []byte) {
	sizeField, pathField, ok := bytes.Cut(line, []byte{'\t'})
	if !ok {
		t.garbled = true
		return
	}
	kb, err := strconv.ParseInt(string(sizeField), 10, 64)
	if err != nil {
		t.garbled = true
		return
	}
	path := string(pathField)
	switch {
	case path == t.root:
		t.rootKB, t.sawRoot = kb, true
	case !strings.HasPrefix(path, t.root+"/"):
		t.garbled = true
		return
	}
	inside := path + "/"
	for n := len(t.finished); n > 0 && strings.HasPrefix(t.finished[n-1].path, inside); n-- {
		t.sumKB -= t.finished[n-1].kb
		t.finished = t.finished[:n-1]
	}
	t.finished = append(t.finished, duSubtree{path: path, kb: kb})
	t.sumKB += kb
}

// bytes returns the bytes du had fully counted so far. It is a lower bound
// only while garbled is false.
func (t *duSubtreeTally) bytes() int64 { return t.sumKB * 1024 }

// duComplaints keeps du's stderr for an error message: whether any line was a
// permission failure, and the start of the text.
type duComplaints struct {
	denied bool
	text   strings.Builder
}

func (c *duComplaints) add(line []byte) {
	if len(line) == 0 {
		return
	}
	if bytes.Contains(line, []byte("Permission denied")) {
		c.denied = true
	}
	if c.text.Len() >= duComplaintsLimit {
		return
	}
	if c.text.Len() > 0 {
		c.text.WriteString(" ; ")
	}
	c.text.Write(line)
}
