package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	gitSectionOutputLimit     = 64 * 1024
	gitRenderedOutputLimit    = 128 * 1024
	gitErrorOutputLimit       = 4 * 1024
	gitTruncationMarker       = "\n...[truncated]\n"
	gitRevisionMinimumVersion = "git 2.30 or later is required for /git <revision>"
)

var (
	gitCommandTimeout = 10 * time.Second
	gitVersionPattern = regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)(?:[.-][0-9A-Za-z][0-9A-Za-z.+-]*)?(?: \([^\r\n]*\))?$`)
)

func limitGitOutput(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + gitTruncationMarker
}

// runGitLimited executes git with bounded output and a per-command timeout.
func runGitLimited(ctx context.Context, dir string, maxBytes int, args ...string) (string, error) {
	if maxBytes <= 0 {
		return "", errors.New("invalid git output limit")
	}
	if len(args) == 0 {
		return "", errors.New("missing git command")
	}
	gitCtx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	gitArgs := []string{
		"-c", "color.ui=false",
		"-c", "core.pager=cat",
		"-c", "diff.external=",
		"-c", "core.fsmonitor=false",
		"--no-pager",
	}
	gitArgs = append(gitArgs, args...)
	cmd := exec.CommandContext(gitCtx, "git", gitArgs...)
	cmd.Dir = dir
	cancellation, err := newGitCommandCancellation(cmd)
	if err != nil {
		return "", fmt.Errorf("configure git cancellation: %w", err)
	}
	defer func() {
		_ = cancellation.close()
	}()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("prepare git command: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("prepare git command: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start git command: %w", err)
	}
	if err := cancellation.attach(cmd); err != nil {
		_ = cancellation.terminate()
		_ = cmd.Process.Kill()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = cmd.Wait()
		return "", fmt.Errorf("configure git cancellation: %w", err)
	}

	type gitReadResult struct {
		output []byte
		err    error
	}
	stdoutResult := make(chan gitReadResult, 1)
	go func() {
		output, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes+1)))
		stdoutResult <- gitReadResult{output: output, err: readErr}
	}()
	stderrResult := make(chan gitReadResult, 1)
	go func() {
		output, readErr := io.ReadAll(io.LimitReader(stderr, int64(gitErrorOutputLimit+1)))
		stderrResult <- gitReadResult{output: output, err: readErr}
	}()

	var stdoutRead, stderrRead gitReadResult
	var gotStdout, gotStderr, outputTruncated, stderrTruncated, stopped bool
	contextDone := gitCtx.Done()
	stopCommand := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cancellation.terminate()
		_ = stdout.Close()
		_ = stderr.Close()
		contextDone = nil
	}
	for !gotStdout || !gotStderr {
		select {
		case stdoutRead = <-stdoutResult:
			gotStdout = true
			outputTruncated = len(stdoutRead.output) > maxBytes
			if outputTruncated {
				stopCommand()
			}
		case stderrRead = <-stderrResult:
			gotStderr = true
			stderrTruncated = len(stderrRead.output) > gitErrorOutputLimit
			if stderrTruncated {
				stopCommand()
			}
		case <-contextDone:
			stopCommand()
		}
	}
	waitErr := cmd.Wait()
	if errors.Is(gitCtx.Err(), context.DeadlineExceeded) {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git %s timed out: %w", args[0], ctx.Err())
		}
		return "", fmt.Errorf("git %s timed out after %s", args[0], gitCommandTimeout)
	}
	if errors.Is(gitCtx.Err(), context.Canceled) {
		return "", fmt.Errorf("git %s canceled", args[0])
	}
	if outputTruncated {
		return string(stdoutRead.output[:maxBytes]) + gitTruncationMarker, nil
	}
	if stdoutRead.err != nil && !stderrTruncated {
		return "", fmt.Errorf("read git output: %w", stdoutRead.err)
	}
	if stderrRead.err != nil {
		return "", fmt.Errorf("read git output: %w", stderrRead.err)
	}
	if waitErr != nil {
		detail := strings.TrimSpace(string(stderrRead.output))
		if stderrTruncated {
			detail += gitTruncationMarker
		}
		if detail != "" {
			return "", fmt.Errorf("git %s failed: %w: %s", args[0], waitErr, detail)
		}
		return "", fmt.Errorf("git %s failed: %w", args[0], waitErr)
	}
	return string(stdoutRead.output), nil
}

func gitAvailableError(err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("git not available")
	}
	return err
}

func gitCommandInterrupted(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, " timed out") || strings.Contains(message, " canceled")
}

func gitStatusHasChanges(status string) bool {
	for _, line := range strings.Split(status, "\n") {
		if line != "" && !strings.HasPrefix(line, "## ") {
			return true
		}
	}
	return false
}

func appendGitSection(b *strings.Builder, title, content, empty string) {
	b.WriteString(title)
	b.WriteString(":\n")
	content = strings.TrimRight(content, "\r\n")
	if strings.TrimSpace(content) == "" {
		b.WriteString(empty)
		b.WriteString("\n")
		return
	}
	b.WriteString(content)
	b.WriteString("\n")
}

func gitRepositoryRoot(ctx context.Context, dir string) (string, error) {
	root, err := runGitLimited(ctx, dir, 4096, "rev-parse", "--show-toplevel")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git not available")
		}
		if gitCommandInterrupted(err) {
			return "", err
		}
		return "", errors.New("not inside a git repository")
	}
	return strings.TrimSpace(root), nil
}

func gitBranchSummary(ctx context.Context, dir string) (string, error) {
	branch, err := runGitLimited(ctx, dir, 4096, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil {
		return strings.TrimSpace(branch), nil
	}
	if gitCommandInterrupted(err) {
		return "", err
	}
	head, headErr := runGitLimited(ctx, dir, 4096, "rev-parse", "--short", "HEAD")
	if headErr != nil {
		return "", gitAvailableError(headErr)
	}
	return "HEAD detached at " + strings.TrimSpace(head), nil
}

func gitOverview(ctx context.Context, dir string) (string, error) {
	root, err := gitRepositoryRoot(ctx, dir)
	if err != nil {
		return "", err
	}
	branch, err := gitBranchSummary(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("git branch lookup failed: %w", err)
	}
	status, err := runGitLimited(ctx, dir, gitSectionOutputLimit,
		"status", "--short", "--branch", "--untracked-files=all")
	if err != nil {
		return "", gitAvailableError(err)
	}
	staged, err := runGitLimited(ctx, dir, gitSectionOutputLimit,
		"diff", "--cached", "--stat", "--patch", "--unified=3", "--no-ext-diff", "--no-color", "--no-textconv")
	if err != nil {
		return "", gitAvailableError(err)
	}
	unstaged, err := runGitLimited(ctx, dir, gitSectionOutputLimit,
		"diff", "--stat", "--patch", "--unified=3", "--no-ext-diff", "--no-color", "--no-textconv")
	if err != nil {
		return "", gitAvailableError(err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Repository root: %s\nBranch: %s\n\n", root, branch)
	appendGitSection(&b, "Status", status, "(no status)")
	if !gitStatusHasChanges(status) {
		b.WriteString("(no changes)\n")
	}
	b.WriteString("\n")
	appendGitSection(&b, "Staged changes", staged, "(no staged changes)")
	b.WriteString("\n")
	appendGitSection(&b, "Unstaged changes", unstaged, "(no unstaged changes)")
	return limitGitOutput(b.String(), gitRenderedOutputLimit), nil
}

type gitVersion struct {
	major int
	minor int
}

func parseGitVersion(output string) (gitVersion, error) {
	matches := gitVersionPattern.FindStringSubmatch(strings.TrimSpace(output))
	if matches == nil {
		return gitVersion{}, fmt.Errorf("unrecognized git version output: %q", strings.TrimSpace(output))
	}
	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return gitVersion{}, fmt.Errorf("parse git major version: %w", err)
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return gitVersion{}, fmt.Errorf("parse git minor version: %w", err)
	}
	return gitVersion{major: major, minor: minor}, nil
}

func (version gitVersion) atLeast(major, minor int) bool {
	return version.major > major || (version.major == major && version.minor >= minor)
}

func gitInstalledVersion(ctx context.Context, dir string) (gitVersion, error) {
	output, err := runGitLimited(ctx, dir, 4096, "version")
	if err != nil {
		return gitVersion{}, err
	}
	return parseGitVersion(output)
}

func gitRevision(ctx context.Context, dir, revision string) (string, error) {
	if _, err := gitRepositoryRoot(ctx, dir); err != nil {
		return "", err
	}
	version, err := gitInstalledVersion(ctx, dir)
	if err != nil {
		if gitCommandInterrupted(err) {
			return "", err
		}
		return "", errors.New(gitRevisionMinimumVersion)
	}
	if !version.atLeast(2, 30) {
		return "", errors.New(gitRevisionMinimumVersion)
	}
	resolved, err := runGitLimited(ctx, dir, 4096,
		"rev-parse", "--verify", "--quiet", "--end-of-options", revision+"^{commit}")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git not available")
		}
		if gitCommandInterrupted(err) {
			return "", err
		}
		return "", fmt.Errorf("invalid git revision: %s", revision)
	}
	shaFields := strings.Fields(resolved)
	if len(shaFields) != 1 {
		return "", fmt.Errorf("invalid git revision: %s", revision)
	}
	sha := shaFields[0]
	metadata, err := runGitLimited(ctx, dir, gitSectionOutputLimit,
		"show", "--no-patch", "--format=fuller", "--decorate=short", "--no-ext-diff", "--no-color", sha)
	if err != nil {
		return "", gitAvailableError(err)
	}
	patch, err := runGitLimited(ctx, dir, gitSectionOutputLimit,
		"show", "--stat", "--patch", "--unified=3", "--no-ext-diff", "--no-color", "--no-textconv", "--format=", sha)
	if err != nil {
		return "", gitAvailableError(err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Resolved revision: %s\n\n", sha)
	appendGitSection(&b, "Commit metadata", metadata, "(no commit metadata)")
	b.WriteString("\n")
	appendGitSection(&b, "Commit changes", patch, "(no changes in this commit)")
	return limitGitOutput(b.String(), gitRenderedOutputLimit), nil
}

func handleGitREPLCommand(ctx context.Context, revisions []string) (string, error) {
	if len(revisions) > 1 {
		return "", errors.New("usage: /git [revision]")
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine current directory: %w", err)
	}
	if len(revisions) == 1 {
		return gitRevision(ctx, dir, revisions[0])
	}
	return gitOverview(ctx, dir)
}
