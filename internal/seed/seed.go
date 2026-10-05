// Package seed copies per-checkout files (the ones git does not carry:
// .envrc, local *.mk, helper scripts) into a new worktree, templating a
// few placeholders so each copy can point at its own workstream.
package seed

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Vars are the template values.
type Vars struct {
	ID, Name, Repo, Dir, CodeDir, Home string
}

func (v Vars) replacer() *strings.Replacer {
	return strings.NewReplacer(
		"{{id}}", v.ID, "{{name}}", v.Name, "{{repo}}", v.Repo,
		"{{dir}}", v.Dir, "{{code_dir}}", v.CodeDir, "{{home}}", v.Home,
	)
}

// Apply copies every file under seedDir into dst at the same relative path.
// Existing files are kept unless force. Text files are templated; files
// containing NUL bytes are copied verbatim. It returns the relative paths
// written.
func Apply(seedDir, dst string, v Vars, force bool) ([]string, error) {
	var written []string
	r := v.replacer()
	err := filepath.WalkDir(seedDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(seedDir, p)
		out := filepath.Join(dst, rel)
		if _, err := os.Stat(out); err == nil && !force {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.ContainsRune(b, 0) {
			b = []byte(r.Replace(string(b)))
		}
		fi, _ := d.Info()
		mode := fs.FileMode(0o644)
		if fi != nil {
			mode = fi.Mode().Perm()
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, b, mode); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	})
	return written, err
}

// DirenvAllow trusts dir's .envrc when direnv is installed; a no-op
// otherwise.
func DirenvAllow(dir string) error {
	if _, err := exec.LookPath("direnv"); err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, ".envrc")); err != nil {
		return nil
	}
	out, err := exec.Command("direnv", "allow", dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("direnv allow: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
