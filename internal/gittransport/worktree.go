package gittransport

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CommitIdentity is used for the automated rewrite commit.
type CommitIdentity struct {
	Name  string
	Email string
}

// CheckoutBranch does a shallow single-branch clone of url@branch into dir.
func CheckoutBranch(url, branch, dir string) error {
	_ = os.RemoveAll(dir)
	args := []string{"clone", "--depth", "1", "--single-branch"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, url, dir)
	if _, err := runGit("", args...); err != nil {
		return err
	}
	return nil
}

// CommitAll stages every change and creates a commit. It reports whether a
// commit was actually created (false when the tree was already clean).
func CommitAll(dir, message string, id CommitIdentity) (bool, error) {
	if _, err := runGit(dir, "add", "-A"); err != nil {
		return false, err
	}
	// Nothing staged?
	if _, err := runGit(dir, "diff", "--cached", "--quiet"); err == nil {
		return false, nil
	}
	name := id.Name
	if name == "" {
		name = "gitlab-copy-tool"
	}
	email := id.Email
	if email == "" {
		email = "gitlab-copy-tool@localhost"
	}
	cmd := exec.Command("git",
		"-c", "user.name="+name,
		"-c", "user.email="+email,
		"commit", "-m", message)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(),
		"GIT_AUTHOR_NAME="+name, "GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME="+name, "GIT_COMMITTER_EMAIL="+email,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("commit: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// PushBranch pushes the given branch to url.
func PushBranch(dir, url, branch string) error {
	_, err := runGit(dir, "push", url, "HEAD:refs/heads/"+branch)
	return err
}
