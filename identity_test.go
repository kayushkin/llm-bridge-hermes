package main

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestHarnessIdentityIsSelfConsistent pins every place this wrapper commits to a
// harness identity to a single value: the harness this checkout actually is.
//
//	checkout directory name        which harness this repo is
//	module path in go.mod          who we are as a Go package
//	msg.HarnessBinaryName(harnessIdentity) the binary name llm-bridge-server resolves on PATH
//	BIN_NAME in deploy.sh          what ./deploy.sh builds, installs and restarts
//
// These are independent constants. Every wrapper in this family was created by
// cloning an existing one, which copies them all verbatim, and any one left
// un-retargeted silently points this harness at a different harness: it answers
// to that harness's name on the bus, or overwrites its installed binary. Both
// shipped in llm-bridge-copilotcli (a claudecode clone) and neither was caught by
// a build or a test, because the wrong values all agreed with each other — only
// the checkout directory disagreed, which is why it is the anchor here.
//
// Wrappers that carry a state.db (claudecode, codex, jig) pin its directory too;
// this one holds no session-chain state, so there is no state leg to check.
func TestHarnessIdentityIsSelfConsistent(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	want := identityAnchor(t, root)
	if !strings.HasPrefix(want, "llm-bridge-") {
		t.Skipf("checkout dir %q is not a canonical llm-bridge-* checkout; no anchor to compare identity against", want)
	}

	if got := msg.HarnessBinaryName(harnessIdentity); got != want {
		t.Errorf("harness constant is %q, whose binary name is %q, want %q\n"+
			"this wrapper stamps %q on every event it emits and every session it discovers",
			harnessIdentity, got, want, harnessIdentity)
	}

	if got := moduleBase(t, root); got != want {
		t.Errorf("go.mod module path base = %q, want %q", got, want)
	}

	if got, ok := deployBinName(t, root); ok && got != want {
		t.Errorf("deploy.sh BIN_NAME = %q, want %q\n"+
			"./deploy.sh would install this build over the %q binary and restart its service",
			got, want, got)
	}
}

var moduleLine = regexp.MustCompile(`(?m)^module\s+(\S+)`)

// moduleBase returns the last path element of the module path declared in go.mod.
func moduleBase(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := moduleLine.FindSubmatch(b)
	if m == nil {
		t.Fatalf("no module line in go.mod")
	}
	return path.Base(string(m[1]))
}

var binNameLine = regexp.MustCompile(`(?m)^BIN_NAME=["']?([^"'\s]+)`)

// deployBinName returns the BIN_NAME deploy.sh installs under. The second result
// is false when this wrapper ships no deploy.sh, or none that sets BIN_NAME.
func deployBinName(t *testing.T, root string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "deploy.sh"))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read deploy.sh: %v", err)
	}
	m := binNameLine.FindSubmatch(b)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// identityAnchor returns the name of the directory this source was cloned into.
// That name is what every leg above is compared against, so getting it right is
// the whole premise of the test.
//
// In an ordinary checkout it is simply the working directory. In a git worktree
// it is not. A worktree is a second view of the same clone, checked out into a
// directory whose name whoever created it invented — and that invented name
// still starts with "llm-bridge-", so the prefix check above waves it through.
// Every leg then compares a correctly-retargeted constant against a name no
// constant in the repository was ever meant to match, and all four fail. Git
// knows where the clone itself lives: --git-common-dir names the clone's .git
// directory even when asked from inside a worktree. So ask git which checkout
// this is, rather than reading the path we happen to be standing in.
func identityAnchor(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		t.Skipf("cannot ask git which checkout %q belongs to (%v); without that answer a worktree is "+
			"indistinguishable from a mis-named clone, and every assertion below would fail for the wrong reason", root, err)
		return "" // unreachable; Skipf ends the test
	}
	return filepath.Base(filepath.Dir(strings.TrimSpace(string(out))))
}
