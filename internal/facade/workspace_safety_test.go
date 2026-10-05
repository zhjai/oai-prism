package facade

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/oai-prism/oaiprism/internal/prism"
)

func TestModifiedPrependPreservesExistingFile(t *testing.T) {
	for name, diff := range map[string]json.RawMessage{
		"unified": diffRaw("@@ -0,0 +1 @@\n+header\n"),
		"object":  json.RawMessage(`{"version":1,"hunks":[{"original":"","updated":"header\n","location":{"originalStartLine":0,"originalLineCount":0}}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			file := prism.CodexDeltaFile{FilePath: "a.txt", Status: "modified", Diff: diff}
			plan, err := planDeltaFile(file)
			if err != nil || plan.Kind != editPatch {
				t.Fatalf("prepend must be a patch: %+v, %v", plan, err)
			}
			for _, method := range []string{"workspace", "generated_command"} {
				t.Run(method, func(t *testing.T) {
					root := t.TempDir()
					path := filepath.Join(root, "a.txt")
					if err := os.WriteFile(path, []byte("original\nlast line\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					if method == "workspace" {
						if err := ApplyLocalWorkspaceFiles(root, []prism.CodexDeltaFile{file}); err != nil {
							t.Fatal(err)
						}
					} else {
						bash, err := exec.LookPath("bash")
						if err != nil {
							t.Skip("bash unavailable")
						}
						if _, err := exec.LookPath("python3"); err != nil {
							t.Skip("python3 unavailable")
						}
						for _, command := range execCmds(t, SynthesizeDeltaFilesExecJS([]prism.CodexDeltaFile{file}, false)) {
							cmd := exec.Command(bash, "-c", command)
							cmd.Dir = root
							if out, err := cmd.CombinedOutput(); err != nil {
								t.Fatalf("generated patch failed: %v: %s", err, out)
							}
						}
					}
					got, err := os.ReadFile(path)
					if err != nil || string(got) != "header\noriginal\nlast line\n" {
						t.Fatalf("original content lost: %q, %v", got, err)
					}
				})
			}
		})
	}
}

func TestLocalWorkspaceRejectsSymlinkEscape(t *testing.T) {
	for _, linkType := range []string{"file", "directory"} {
		for _, operation := range []string{"added", "modified", "deleted"} {
			t.Run(linkType+"/"+operation, func(t *testing.T) {
				root, outside := t.TempDir(), t.TempDir()
				victim := filepath.Join(outside, "victim.txt")
				if err := os.WriteFile(victim, []byte("original\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				target, relative := victim, "link"
				if linkType == "directory" {
					target, relative = outside, "link/victim.txt"
				}
				if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				diff := diffRaw("@@ -0,0 +1 @@\n+replacement\n")
				if operation == "modified" {
					diff = diffRaw("@@ -1 +1 @@\n-original\n+replacement\n")
				}
				err := ApplyLocalWorkspaceFiles(root, []prism.CodexDeltaFile{{FilePath: relative, Status: operation, Diff: diff}})
				// Deleting a file symlink is safe: Remove unlinks it without touching its target.
				if !(operation == "deleted" && linkType == "file") && err == nil {
					t.Fatal("operation through an escaping symlink must fail")
				}
				got, err := os.ReadFile(victim)
				if err != nil || string(got) != "original\n" {
					t.Fatalf("external file changed: %q, %v", got, err)
				}
			})
		}
	}
}

func TestLocalWorkspaceAllowsContainedSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "actual"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("actual", filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	file := prism.CodexDeltaFile{FilePath: "alias/sub/a.txt", Status: "added", Diff: diffRaw("@@ -0,0 +1 @@\n+inside\n")}
	if err := ApplyLocalWorkspaceFiles(root, []prism.CodexDeltaFile{file}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "actual", "sub", "a.txt"))
	if err != nil || string(got) != "inside\n" {
		t.Fatalf("contained symlink write failed: %q, %v", got, err)
	}
}
