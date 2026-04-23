// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/log"
)

// RawDiffType type of a raw diff.
type RawDiffType string

// RawDiffType possible values.
const (
	RawDiffNormal RawDiffType = "diff"
	RawDiffPatch  RawDiffType = "patch"
)

// GetRawDiff dumps diff results of repository in given commit ID to io.Writer.
func GetRawDiff(repo *Repository, commitID string, diffType RawDiffType, writer io.Writer) error {
	return GetRepoRawDiffForFile(repo, "", commitID, diffType, "", writer)
}

// GetReverseRawDiff dumps the reverse diff results of repository in given commit ID to io.Writer.
func GetReverseRawDiff(ctx context.Context, repoPath, commitID string, writer io.Writer) error {
	stderr := new(bytes.Buffer)
	cmd := NewCommand(ctx, "show", "--pretty=format:revert %H%n", "-R").AddDynamicArguments(commitID)
	if err := cmd.Run(&RunOpts{
		Dir:    repoPath,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// GetRepoRawDiffForFile dumps diff results of file in given commit ID to io.Writer according given repository
func GetRepoRawDiffForFile(repo *Repository, startCommit, endCommit string, diffType RawDiffType, file string, writer io.Writer) error {
	commit, err := repo.GetCommit(endCommit)
	if err != nil {
		return err
	}
	var files []string
	if len(file) > 0 {
		files = append(files, file)
	}

	cmd := NewCommand(repo.Ctx)
	switch diffType {
	case RawDiffNormal:
		if len(startCommit) != 0 {
			cmd.AddArguments("diff", "-M").AddDynamicArguments(startCommit, endCommit).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("show").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			cmd.AddArguments("diff", "-M").AddDynamicArguments(c.ID.String(), endCommit).AddDashesAndList(files...)
		}
	case RawDiffPatch:
		if len(startCommit) != 0 {
			query := fmt.Sprintf("%s...%s", endCommit, startCommit)
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(query).AddDashesAndList(files...)
		} else if commit.ParentCount() == 0 {
			cmd.AddArguments("format-patch", "--no-signature", "--stdout", "--root").AddDynamicArguments(endCommit).AddDashesAndList(files...)
		} else {
			c, _ := commit.Parent(0)
			query := fmt.Sprintf("%s...%s", endCommit, c.ID.String())
			cmd.AddArguments("format-patch", "--no-signature", "--stdout").AddDynamicArguments(query).AddDashesAndList(files...)
		}
	default:
		return fmt.Errorf("invalid diffType: %s", diffType)
	}

	stderr := new(bytes.Buffer)
	if err = cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: writer,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("Run: %w - %s", err, stderr)
	}
	return nil
}

// ParseDiffHunkString parse the diffhunk content and return
func ParseDiffHunkString(diffhunk string) (leftLine, leftHunk, rightLine, righHunk int) {
	ss := strings.Split(diffhunk, "@@")
	ranges := strings.Split(ss[1][1:], " ")
	leftRange := strings.Split(ranges[0], ",")
	leftLine, _ = strconv.Atoi(leftRange[0][1:])
	if len(leftRange) > 1 {
		leftHunk, _ = strconv.Atoi(leftRange[1])
	}
	if len(ranges) > 1 {
		rightRange := strings.Split(ranges[1], ",")
		rightLine, _ = strconv.Atoi(rightRange[0])
		if len(rightRange) > 1 {
			righHunk, _ = strconv.Atoi(rightRange[1])
		}
	} else {
		log.Debug("Parse line number failed: %v", diffhunk)
		rightLine = leftLine
		righHunk = leftHunk
	}
	return leftLine, leftHunk, rightLine, righHunk
}

// Example: @@ -1,8 +1,9 @@ => [..., 1, 8, 1, 9]
var hunkRegex = regexp.MustCompile(`^@@ -(?P<beginOld>[0-9]+)(,(?P<endOld>[0-9]+))? \+(?P<beginNew>[0-9]+)(,(?P<endNew>[0-9]+))? @@`)

const cmdDiffHead = "diff --git "

func isHeader(lof string, inHunk bool) bool {
	return strings.HasPrefix(lof, cmdDiffHead) || (!inHunk && (strings.HasPrefix(lof, "---") || strings.HasPrefix(lof, "+++")))
}

// CutDiffAroundLine cuts a diff of a file in way that only the given line + numberOfLine above it will be shown
// it also recalculates hunks and adds the appropriate headers to the new diff.
// Warning: Only one-file diffs are allowed.
func CutDiffAroundLine(originalDiff io.Reader, line int64, old bool, numbersOfLine int) (string, error) {
	if line == 0 || numbersOfLine == 0 {
		// no line or num of lines => no diff
		return "", nil
	}

	scanner := bufio.NewScanner(originalDiff)
	hunk := make([]string, 0)

	// begin is the start of the hunk containing searched line
	// end is the end of the hunk ...
	// currentLine is the line number on the side of the searched line (differentiated by old)
	// otherLine is the line number on the opposite side of the searched line (differentiated by old)
	var begin, end, currentLine, otherLine int64
	var headerLines int

	inHunk := false

	for scanner.Scan() {
		lof := scanner.Text()
		// Add header to enable parsing

		if isHeader(lof, inHunk) {
			if strings.HasPrefix(lof, cmdDiffHead) {
				inHunk = false
			}
			hunk = append(hunk, lof)
			headerLines++
		}
		if currentLine > line {
			break
		}
		// Detect "hunk" with contains commented lof
		if strings.HasPrefix(lof, "@@") {
			inHunk = true
			// Already got our hunk. End of hunk detected!
			if len(hunk) > headerLines {
				break
			}
			// A map with named groups of our regex to recognize them later more easily
			submatches := hunkRegex.FindStringSubmatch(lof)
			groups := make(map[string]string)
			for i, name := range hunkRegex.SubexpNames() {
				if i != 0 && name != "" {
					groups[name] = submatches[i]
				}
			}
			if old {
				begin, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
				end, _ = strconv.ParseInt(groups["endOld"], 10, 64)
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
			} else {
				begin, _ = strconv.ParseInt(groups["beginNew"], 10, 64)
				if groups["endNew"] != "" {
					end, _ = strconv.ParseInt(groups["endNew"], 10, 64)
				} else {
					end = 0
				}
				// init otherLine with begin of opposite side
				otherLine, _ = strconv.ParseInt(groups["beginOld"], 10, 64)
			}
			end += begin // end is for real only the number of lines in hunk
			// lof is between begin and end
			if begin <= line && end >= line {
				hunk = append(hunk, lof)
				currentLine = begin
				continue
			}
		} else if len(hunk) > headerLines {
			hunk = append(hunk, lof)
			// Count lines in context
			switch lof[0] {
			case '+':
				if !old {
					currentLine++
				} else {
					otherLine++
				}
			case '-':
				if old {
					currentLine++
				} else {
					otherLine++
				}
			case '\\':
				// FIXME: handle `\ No newline at end of file`
			default:
				currentLine++
				otherLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// No hunk found
	if currentLine == 0 {
		return "", nil
	}
	// headerLines + hunkLine (1) = totalNonCodeLines
	if len(hunk)-headerLines-1 <= numbersOfLine {
		// No need to cut the hunk => return existing hunk
		return strings.Join(hunk, "\n"), nil
	}
	var oldBegin, oldNumOfLines, newBegin, newNumOfLines int64
	if old {
		oldBegin = currentLine
		newBegin = otherLine
	} else {
		oldBegin = otherLine
		newBegin = currentLine
	}
	// headers + hunk header
	newHunk := make([]string, headerLines)
	// transfer existing headers
	copy(newHunk, hunk[:headerLines])
	// transfer last n lines
	newHunk = append(newHunk, hunk[len(hunk)-numbersOfLine-1:]...)
	// calculate newBegin, ... by counting lines
	for i := len(hunk) - 1; i >= len(hunk)-numbersOfLine; i-- {
		switch hunk[i][0] {
		case '+':
			newBegin--
			newNumOfLines++
		case '-':
			oldBegin--
			oldNumOfLines++
		default:
			oldBegin--
			newBegin--
			newNumOfLines++
			oldNumOfLines++
		}
	}
	// construct the new hunk header
	newHunk[headerLines] = fmt.Sprintf("@@ -%d,%d +%d,%d @@",
		oldBegin, oldNumOfLines, newBegin, newNumOfLines)
	return strings.Join(newHunk, "\n"), nil
}

// GetAffectedFiles returns the affected files between two commits
func GetAffectedFiles(repo *Repository, branchName, oldCommitID, newCommitID string, env []string) ([]string, error) {
	if oldCommitID == emptySha1ObjectID.String() || oldCommitID == emptySha256ObjectID.String() {
		startCommitID, err := repo.GetCommitBranchStart(env, branchName, newCommitID)
		if err != nil {
			return nil, err
		}
		if startCommitID == "" {
			return nil, fmt.Errorf("cannot find the start commit of %s", newCommitID)
		}
		oldCommitID = startCommitID
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return nil, err
	}
	// Copyright 2019 The Gitea Authors.
// All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"code.gitea.io/gitea/models/db"
	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/system"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/optional"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/gitverse"
	notify_service "code.gitea.io/gitea/services/notify"
)

var notEnoughLines = regexp.MustCompile(`fatal: file .* has only \d+ lines?`)

// ErrDismissRequestOnClosedPR represents an error when an user tries to dismiss a review associated to a closed or merged PR.
type ErrDismissRequestOnClosedPR struct{}

// IsErrDismissRequestOnClosedPR checks if an error is an ErrDismissRequestOnClosedPR.
func IsErrDismissRequestOnClosedPR(err error) bool {
	_, ok := err.(ErrDismissRequestOnClosedPR)
	return ok
}

func (err ErrDismissRequestOnClosedPR) Error() string {
	return "can't dismiss a review associated to a closed or merged PR"
}

func (err ErrDismissRequestOnClosedPR) Unwrap() error {
	return util.ErrPermissionDenied
}

// ErrSubmitReviewOnClosedPR represents an error when an user tries to submit an approve or reject review associated to a closed or merged PR.
var ErrSubmitReviewOnClosedPR = errors.New("can't submit review for a closed or merged PR")

// checkInvalidation checks if the line of code comment got changed by another commit.
// If the line got changed the comment is going to be invalidated.
func checkInvalidation(ctx context.Context, c *issues_model.Comment, repo *git.Repository, branch string) error {
	// FIXME differentiate between previous and proposed line
	commit, err := repo.LineBlame(branch, repo.Path, c.TreePath, uint(c.UnsignedLine()))
	if err != nil && (strings.Contains(err.Error(), "fatal: no such path") || notEnoughLines.MatchString(err.Error())) {
		c.Invalidated = true
		return issues_model.UpdateCommentInvalidate(ctx, c)
	}
	if err != nil {
		return err
	}
	if c.CommitSHA != "" && c.CommitSHA != commit.ID.String() {
		c.Invalidated = true
		return issues_model.UpdateCommentInvalidate(ctx, c)
	}
	return nil
}

// InvalidateCodeComments will lookup the prs for code comments which got invalidated by change
func InvalidateCodeComments(ctx context.Context, prs issues_model.PullRequestList, doer *user_model.User, repo *git.Repository, branch string) error {
	if len(prs) == 0 {
		return nil
	}
	issueIDs := prs.GetIssueIDs()

	codeComments, err := db.Find[issues_model.Comment](ctx, issues_model.FindCommentsOptions{
		ListOptions: db.ListOptionsAll,
		Type:        issues_model.CommentTypeCode,
		Invalidated: optional.Some(false),
		IssueIDs:    issueIDs,
	})
	if err != nil {
		return fmt.Errorf("find code comments: %v", err)
	}
	for _, comment := range codeComments {
		if err := checkInvalidation(ctx, comment, repo, branch); err != nil {
			return err
		}
	}
	return nil
}

// CreateCodeComment creates a comment on the code line
func CreateCodeComment(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, issue *issues_model.Issue, line int64, content, treePath string, pendingReview bool, replyReviewID int64, latestCommitID string, attachments []string) (*issues_model.Comment, error) {
	var (
		existsReview bool
		err          error
	)

	// CreateCodeComment() is used for:
	// - Single comments
	// - Comments that are part of a review
	// - Comments that reply to an existing review

	if !pendingReview && replyReviewID != 0 {
		// It's not part of a review; maybe a reply to a review comment or a single comment.
		// Check if there are reviews for that line already; if there are, this is a reply
		if existsReview, err = issues_model.ReviewExists(ctx, issue, treePath, line); err != nil {
			return nil, err
		}
	}

	// Comments that are replies don't require a review header to show up in the issue view
	if !pendingReview && existsReview {
		if err = issue.LoadRepo(ctx); err != nil {
			return nil, err
		}

		comment, err := createCodeComment(ctx,
			doer,
			issue.Repo,
			issue,
			content,
			treePath,
			line,
			replyReviewID,
			attachments,
		)
		if err != nil {
			return nil, err
		}

		mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, comment.Content)
		if err != nil {
			return nil, err
		}

		notify_service.CreateIssueComment(ctx, doer, issue.Repo, issue, comment, mentions)

		return comment, nil
	}

	review, err := issues_model.GetCurrentReview(ctx, doer, issue)
	if err != nil {
		if !issues_model.IsErrReviewNotExist(err) {
			return nil, err
		}

		if review, err = issues_model.CreateReview(ctx, issues_model.CreateReviewOptions{
			Type:     issues_model.ReviewTypePending,
			Reviewer: doer,
			Issue:    issue,
			Official: false,
			CommitID: latestCommitID,
		}); err != nil {
			return nil, err
		}
	}

	comment, err := createCodeComment(ctx,
		doer,
		issue.Repo,
		issue,
		content,
		treePath,
		line,
		review.ID,
		attachments,
	)
	comment.CommitSHA = latestCommitID
	comment.Type = issues_model.CommentTypeComment
	if err != nil {
		return nil, err
	}

	if !pendingReview && !existsReview {
		// Submit the review we've just created so the comment shows up in the issue view
		if _, _, err = SubmitReview(ctx, doer, gitRepo, issue, issues_model.ReviewTypeComment, content, latestCommitID, nil); err != nil {
			return nil, err
		}
	}

	// NOTICE: if it's a pending review the notifications will not be fired until user submit review.

	return comment, nil
}

// createCodeComment creates a plain code comment at the specified line / path
func createCodeComment(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, issue *issues_model.Issue, content, treePath string, line, reviewID int64, attachments []string) (*issues_model.Comment, error) {
	var commitID, patch string
	if err := issue.LoadPullRequest(ctx); err != nil {
		return nil, fmt.Errorf("LoadPullRequest: %w", err)
	}
	pr := issue.PullRequest
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return nil, fmt.Errorf("LoadBaseRepo: %w", err)
	}
	gitRepo, closer, err := gitrepo.RepositoryFromContextOrOpen(ctx, pr.BaseRepo)
	if err != nil {
		return nil, fmt.Errorf("RepositoryFromContextOrOpen: %w", err)
	}
	defer closer.Close()

	invalidated := false
	head := pr.GetGitRefName()
	if line > 0 {
		if reviewID != 0 {
			first, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
				ReviewID: reviewID,
				Line:     line,
				TreePath: treePath,
				Type:     issues_model.CommentTypeCode,
				ListOptions: db.ListOptions{
					PageSize: 1,
					Page:     1,
				},
			})
			if err == nil && len(first) > 0 {
				commitID = first[0].CommitSHA
				invalidated = first[0].Invalidated
				patch = first[0].Patch
			} else if err != nil && !issues_model.IsErrCommentNotExist(err) {
				return nil, fmt.Errorf("Find first comment for %d line %d path %s. Error: %w", reviewID, line, treePath, err)
			} else {
				review, err := issues_model.GetReviewByID(ctx, reviewID)
				if err == nil && len(review.CommitID) > 0 {
					head = review.CommitID
				} else if err != nil && !issues_model.IsErrReviewNotExist(err) {
					return nil, fmt.Errorf("GetReviewByID %d. Error: %w", reviewID, err)
				}
			}
		}

		if len(commitID) == 0 {
			// FIXME validate treePath
			// Get latest commit referencing the commented line
			// No need for get commit for base branch changes
			commit, err := gitRepo.LineBlame(head, gitRepo.Path, treePath, uint(line))
			if err == nil {
				commitID = commit.ID.String()
			} else if !(strings.Contains(err.Error(), "exit status 128 - fatal: no such path") || notEnoughLines.MatchString(err.Error())) {
				return nil, fmt.Errorf("LineBlame[%s, %s, %s, %d]: %w", pr.GetGitRefName(), gitRepo.Path, treePath, line, err)
			}
		}
	}

	// Only fetch diff if comment is review comment
	if len(patch) == 0 && reviewID != 0 {
		headCommitID, err := gitRepo.GetRefCommitID(pr.GetGitRefName())
		if err != nil {
			return nil, fmt.Errorf("GetRefCommitID[%s]: %w", pr.GetGitRefName(), err)
		}
		if len(commitID) == 0 {
			commitID = headCommitID
		}
		reader, writer := io.Pipe()
		defer func() {
			_ = reader.Close()
			_ = writer.Close()
		}()
		go func() {
			if err := git.GetRepoRawDiffForFile(gitRepo, pr.MergeBase, headCommitID, git.RawDiffNormal, treePath, writer); err != nil {
				_ = writer.CloseWithError(fmt.Errorf("GetRawDiffForLine[%s, %s, %s, %s]: %w", gitRepo.Path, pr.MergeBase, headCommitID, treePath, err))
				return
			}
			_ = writer.Close()
		}()

		patch, err = git.CutDiffAroundLine(reader, int64((&issues_model.Comment{Line: line}).UnsignedLine()), line < 0, setting.UI.CodeCommentLines)
		if err != nil {
			log.Error("Error whilst generating patch: %v", err)
			return nil, err
		}
	}
	return issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
		Type:        issues_model.CommentTypeCode,
		Doer:        doer,
		Repo:        repo,
		Issue:       issue,
		Content:     content,
		LineNum:     line,
		TreePath:    treePath,
		CommitSHA:   commitID,
		ReviewID:    reviewID,
		Patch:       patch,
		Invalidated: invalidated,
		Attachments: attachments,
	})
}

// SubmitReview creates a review out of the existing pending review or creates a new one if no pending review exist
func SubmitReview(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, issue *issues_model.Issue, reviewType issues_model.ReviewType, content, commitID string, attachmentUUIDs []string) (*issues_model.Review, *issues_model.Comment, error) {
	isPullRequestThreadsAvailable := gitverse.IsServiceExistForUser(ctx, system.NewDesignActivity, doer)
	if err := issue.LoadPullRequest(ctx); err != nil {
		return nil, nil, err
	}

	pr := issue.PullRequest
	var stale bool
	if reviewType != issues_model.ReviewTypeApprove && reviewType != issues_model.ReviewTypeReject {
		stale = false
	} else {
		if issue.IsClosed {
			return nil, nil, ErrSubmitReviewOnClosedPR
		}

		headCommitID, err := gitRepo.GetRefCommitID(pr.GetGitRefName())
		if err != nil {
			return nil, nil, err
		}

		if headCommitID == commitID {
			stale = false
		} else {
			stale, err = checkIfPRContentChanged(ctx, pr, commitID, headCommitID)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	review, comm, err := issues_model.SubmitReview(ctx, doer, issue, reviewType, content, commitID, stale, attachmentUUIDs, false)
	if err != nil {
		return nil, nil, err
	}
	err = issues_model.UpdateCommentsInReview(ctx, review, comm.CreatedUnix, isPullRequestThreadsAvailable)
	if err != nil {
		return nil, nil, err
	}

	mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, comm.Content)
	if err != nil {
		return nil, nil, err
	}
	notify_service.PullRequestReview(ctx, pr, review, comm, mentions)

	for _, lines := range review.CodeComments {
		for _, comments := range lines {
			for _, codeComment := range comments {
				mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, codeComment.Content)
				if err != nil {
					return nil, nil, err
				}
				notify_service.PullRequestCodeComment(ctx, pr, codeComment, mentions)
			}
		}
	}

	for _, lines := range review.DiscussionComments {
		for _, comments := range lines {
			for _, discussionComment := range comments {
				mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, discussionComment.Content)
				if err != nil {
					return nil, nil, err
				}
				notify_service.PullRequestCodeComment(ctx, pr, discussionComment, mentions)
			}
		}
	}

	return review, comm, nil
}

// DismissApprovalReviews dismiss all approval reviews because of new commits
func DismissApprovalReviews(ctx context.Context, doer *user_model.User, pull *issues_model.PullRequest) error {
	reviews, err := issues_model.FindReviews(ctx, issues_model.FindReviewOptions{
		ListOptions: db.ListOptionsAll,
		IssueID:     pull.IssueID,
		Types:       []issues_model.ReviewType{issues_model.ReviewTypeApprove},
		Dismissed:   optional.Some(false),
	})
	if err != nil {
		return err
	}

	if err := reviews.LoadIssues(ctx); err != nil {
		return err
	}

	return db.WithTx(ctx, func(ctx context.Context) error {
		for _, review := range reviews {
			if err := issues_model.DismissReview(ctx, review, true); err != nil {
				return err
			}

			comment, err := issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
				Doer:     doer,
				Content:  "New commits pushed, approval review dismissed automatically according to repository settings",
				Type:     issues_model.CommentTypeDismissReview,
				ReviewID: review.ID,
				Issue:    review.Issue,
				Repo:     review.Issue.Repo,
			})
			if err != nil {
				return err
			}

			comment.Review = review
			comment.Poster = doer
			comment.Issue = review.Issue

			notify_service.PullReviewDismiss(ctx, doer, review, comment)
		}
		return nil
	})
}

// DismissReview dismissing stale review by repo admin
func DismissReview(ctx context.Context, reviewID, repoID int64, message string, doer *user_model.User, isDismiss, dismissPriors bool) (comment *issues_model.Comment, err error) {
	review, err := issues_model.GetReviewByID(ctx, reviewID)
	if err != nil {
		return nil, err
	}

	if review.Type != issues_model.ReviewTypeApprove && review.Type != issues_model.ReviewTypeReject {
		return nil, fmt.Errorf("not need to dismiss this review because it's type is not Approve or change request")
	}

	// load data for notify
	if err := review.LoadAttributes(ctx); err != nil {
		return nil, err
	}

	// Check if the review's repoID is the one we're currently expecting.
	if review.Issue.RepoID != repoID {
		return nil, fmt.Errorf("reviews's repository is not the same as the one we expect")
	}

	issue := review.Issue

	if issue.IsClosed {
		return nil, ErrDismissRequestOnClosedPR{}
	}

	if issue.IsPull {
		if err := issue.LoadPullRequest(ctx); err != nil {
			return nil, err
		}
		if issue.PullRequest.HasMerged {
			return nil, ErrDismissRequestOnClosedPR{}
		}
	}

	if err := issues_model.DismissReview(ctx, review, isDismiss); err != nil {
		return nil, err
	}

	if dismissPriors {
		reviews, err := issues_model.FindReviews(ctx, issues_model.FindReviewOptions{
			IssueID:    review.IssueID,
			ReviewerID: review.ReviewerID,
			Dismissed:  optional.Some(false),
		})
		if err != nil {
			return nil, err
		}
		for _, oldReview := range reviews {
			if err = issues_model.DismissReview(ctx, oldReview, true); err != nil {
				return nil, err
			}
		}
	}

	if !isDismiss {
		return nil, nil
	}

	if err := review.Issue.LoadAttributes(ctx); err != nil {
		return nil, err
	}

	comment, err = issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
		Doer:     doer,
		Content:  message,
		Type:     issues_model.CommentTypeDismissReview,
		ReviewID: review.ID,
		Issue:    review.Issue,
		Repo:     review.Issue.Repo,
	})
	if err != nil {
		return nil, err
	}

	comment.Review = review
	comment.Poster = doer
	comment.Issue = review.Issue

	notify_service.PullReviewDismiss(ctx, doer, review, comment)

	return comment, nil
}
// Copyright 2019 The Gitea Authors.
// All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"code.gitea.io/gitea/models/db"
	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/system"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/optional"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/gitverse"
	notify_service "code.gitea.io/gitea/services/notify"
)

var notEnoughLines = regexp.MustCompile(`fatal: file .* has only \d+ lines?`)

// ErrDismissRequestOnClosedPR represents an error when an user tries to dismiss a review associated to a closed or merged PR.
type ErrDismissRequestOnClosedPR struct{}

// IsErrDismissRequestOnClosedPR checks if an error is an ErrDismissRequestOnClosedPR.
func IsErrDismissRequestOnClosedPR(err error) bool {
	_, ok := err.(ErrDismissRequestOnClosedPR)
	return ok
}

func (err ErrDismissRequestOnClosedPR) Error() string {
	return "can't dismiss a review associated to a closed or merged PR"
}

func (err ErrDismissRequestOnClosedPR) Unwrap() error {
	return util.ErrPermissionDenied
}

// ErrSubmitReviewOnClosedPR represents an error when an user tries to submit an approve or reject review associated to a closed or merged PR.
var ErrSubmitReviewOnClosedPR = errors.New("can't submit review for a closed or merged PR")

// checkInvalidation checks if the line of code comment got changed by another commit.
// If the line got changed the comment is going to be invalidated.
func checkInvalidation(ctx context.Context, c *issues_model.Comment, repo *git.Repository, branch string) error {
	// FIXME differentiate between previous and proposed line
	commit, err := repo.LineBlame(branch, repo.Path, c.TreePath, uint(c.UnsignedLine()))
	if err != nil && (strings.Contains(err.Error(), "fatal: no such path") || notEnoughLines.MatchString(err.Error())) {
		c.Invalidated = true
		return issues_model.UpdateCommentInvalidate(ctx, c)
	}
	if err != nil {
		return err
	}
	if c.CommitSHA != "" && c.CommitSHA != commit.ID.String() {
		c.Invalidated = true
		return issues_model.UpdateCommentInvalidate(ctx, c)
	}
	return nil
}

// InvalidateCodeComments will lookup the prs for code comments which got invalidated by change
func InvalidateCodeComments(ctx context.Context, prs issues_model.PullRequestList, doer *user_model.User, repo *git.Repository, branch string) error {
	if len(prs) == 0 {
		return nil
	}
	issueIDs := prs.GetIssueIDs()

	codeComments, err := db.Find[issues_model.Comment](ctx, issues_model.FindCommentsOptions{
		ListOptions: db.ListOptionsAll,
		Type:        issues_model.CommentTypeCode,
		Invalidated: optional.Some(false),
		IssueIDs:    issueIDs,
	})
	if err != nil {
		return fmt.Errorf("find code comments: %v", err)
	}
	for _, comment := range codeComments {
		if err := checkInvalidation(ctx, comment, repo, branch); err != nil {
			return err
		}
	}
	return nil
}

// CreateCodeComment creates a comment on the code line
func CreateCodeComment(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, issue *issues_model.Issue, line int64, content, treePath string, pendingReview bool, replyReviewID int64, latestCommitID string, attachments []string) (*issues_model.Comment, error) {
	var (
		existsReview bool
		err          error
	)

	// CreateCodeComment() is used for:
	// - Single comments
	// - Comments that are part of a review
	// - Comments that reply to an existing review

	if !pendingReview && replyReviewID != 0 {
		// It's not part of a review; maybe a reply to a review comment or a single comment.
		// Check if there are reviews for that line already; if there are, this is a reply
		if existsReview, err = issues_model.ReviewExists(ctx, issue, treePath, line); err != nil {
			return nil, err
		}
	}

	// Comments that are replies don't require a review header to show up in the issue view
	if !pendingReview && existsReview {
		if err = issue.LoadRepo(ctx); err != nil {
			return nil, err
		}

		comment, err := createCodeComment(ctx,
			doer,
			issue.Repo,
			issue,
			content,
			treePath,
			line,
			replyReviewID,
			attachments,
		)
		if err != nil {
			return nil, err
		}

		mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, comment.Content)
		if err != nil {
			return nil, err
		}

		notify_service.CreateIssueComment(ctx, doer, issue.Repo, issue, comment, mentions)

		return comment, nil
	}

	review, err := issues_model.GetCurrentReview(ctx, doer, issue)
	if err != nil {
		if !issues_model.IsErrReviewNotExist(err) {
			return nil, err
		}

		if review, err = issues_model.CreateReview(ctx, issues_model.CreateReviewOptions{
			Type:     issues_model.ReviewTypePending,
			Reviewer: doer,
			Issue:    issue,
			Official: false,
			CommitID: latestCommitID,
		}); err != nil {
			return nil, err
		}
	}

	comment, err := createCodeComment(ctx,
		doer,
		issue.Repo,
		issue,
		content,
		treePath,
		line,
		review.ID,
		attachments,
	)
	comment.CommitSHA = latestCommitID
	comment.Type = issues_model.CommentTypeComment
	if err != nil {
		return nil, err
	}

	if !pendingReview && !existsReview {
		// Submit the review we've just created so the comment shows up in the issue view
		if _, _, err = SubmitReview(ctx, doer, gitRepo, issue, issues_model.ReviewTypeComment, content, latestCommitID, nil); err != nil {
			return nil, err
		}
	}

	// NOTICE: if it's a pending review the notifications will not be fired until user submit review.

	return comment, nil
}

// createCodeComment creates a plain code comment at the specified line / path
func createCodeComment(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, issue *issues_model.Issue, content, treePath string, line, reviewID int64, attachments []string) (*issues_model.Comment, error) {
	var commitID, patch string
	if err := issue.LoadPullRequest(ctx); err != nil {
		return nil, fmt.Errorf("LoadPullRequest: %w", err)
	}
	pr := issue.PullRequest
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return nil, fmt.Errorf("LoadBaseRepo: %w", err)
	}
	gitRepo, closer, err := gitrepo.RepositoryFromContextOrOpen(ctx, pr.BaseRepo)
	if err != nil {
		return nil, fmt.Errorf("RepositoryFromContextOrOpen: %w", err)
	}
	defer closer.Close()

	invalidated := false
	head := pr.GetGitRefName()
	if line > 0 {
		if reviewID != 0 {
			first, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
				ReviewID: reviewID,
				Line:     line,
				TreePath: treePath,
				Type:     issues_model.CommentTypeCode,
				ListOptions: db.ListOptions{
					PageSize: 1,
					Page:     1,
				},
			})
			if err == nil && len(first) > 0 {
				commitID = first[0].CommitSHA
				invalidated = first[0].Invalidated
				patch = first[0].Patch
			} else if err != nil && !issues_model.IsErrCommentNotExist(err) {
				return nil, fmt.Errorf("Find first comment for %d line %d path %s. Error: %w", reviewID, line, treePath, err)
			} else {
				review, err := issues_model.GetReviewByID(ctx, reviewID)
				if err == nil && len(review.CommitID) > 0 {
					head = review.CommitID
				} else if err != nil && !issues_model.IsErrReviewNotExist(err) {
					return nil, fmt.Errorf("GetReviewByID %d. Error: %w", reviewID, err)
				}
			}
		}

		if len(commitID) == 0 {
			// FIXME validate treePath
			// Get latest commit referencing the commented line
			// No need for get commit for base branch changes
			commit, err := gitRepo.LineBlame(head, gitRepo.Path, treePath, uint(line))
			if err == nil {
				commitID = commit.ID.String()
			} else if !(strings.Contains(err.Error(), "exit status 128 - fatal: no such path") || notEnoughLines.MatchString(err.Error())) {
				return nil, fmt.Errorf("LineBlame[%s, %s, %s, %d]: %w", pr.GetGitRefName(), gitRepo.Path, treePath, line, err)
			}
		}
	}

	// Only fetch diff if comment is review comment
	if len(patch) == 0 && reviewID != 0 {
		headCommitID, err := gitRepo.GetRefCommitID(pr.GetGitRefName())
		if err != nil {
			return nil, fmt.Errorf("GetRefCommitID[%s]: %w", pr.GetGitRefName(), err)
		}
		if len(commitID) == 0 {
			commitID = headCommitID
		}
		reader, writer := io.Pipe()
		defer func() {
			_ = reader.Close()
			_ = writer.Close()
		}()
		go func() {
			if err := git.GetRepoRawDiffForFile(gitRepo, pr.MergeBase, headCommitID, git.RawDiffNormal, treePath, writer); err != nil {
				_ = writer.CloseWithError(fmt.Errorf("GetRawDiffForLine[%s, %s, %s, %s]: %w", gitRepo.Path, pr.MergeBase, headCommitID, treePath, err))
				return
			}
			_ = writer.Close()
		}()

		patch, err = git.CutDiffAroundLine(reader, int64((&issues_model.Comment{Line: line}).UnsignedLine()), line < 0, setting.UI.CodeCommentLines)
		if err != nil {
			log.Error("Error whilst generating patch: %v", err)
			return nil, err
		}
	}
	return issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
		Type:        issues_model.CommentTypeCode,
		Doer:        doer,
		Repo:        repo,
		Issue:       issue,
		Content:     content,
		LineNum:     line,
		TreePath:    treePath,
		CommitSHA:   commitID,
		ReviewID:    reviewID,
		Patch:       patch,
		Invalidated: invalidated,
		Attachments: attachments,
	})
}

// SubmitReview creates a review out of the existing pending review or creates a new one if no pending review exist
func SubmitReview(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, issue *issues_model.Issue, reviewType issues_model.ReviewType, content, commitID string, attachmentUUIDs []string) (*issues_model.Review, *issues_model.Comment, error) {
	isPullRequestThreadsAvailable := gitverse.IsServiceExistForUser(ctx, system.NewDesignActivity, doer)
	if err := issue.LoadPullRequest(ctx); err != nil {
		return nil, nil, err
	}

	pr := issue.PullRequest
	var stale bool
	if reviewType != issues_model.ReviewTypeApprove && reviewType != issues_model.ReviewTypeReject {
		stale = false
	} else {
		if issue.IsClosed {
			return nil, nil, ErrSubmitReviewOnClosedPR
		}

		headCommitID, err := gitRepo.GetRefCommitID(pr.GetGitRefName())
		if err != nil {
			return nil, nil, err
		}

		if headCommitID == commitID {
			stale = false
		} else {
			stale, err = checkIfPRContentChanged(ctx, pr, commitID, headCommitID)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	review, comm, err := issues_model.SubmitReview(ctx, doer, issue, reviewType, content, commitID, stale, attachmentUUIDs, false)
	if err != nil {
		return nil, nil, err
	}
	err = issues_model.UpdateCommentsInReview(ctx, review, comm.CreatedUnix, isPullRequestThreadsAvailable)
	if err != nil {
		return nil, nil, err
	}

	mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, comm.Content)
	if err != nil {
		return nil, nil, err
	}
	notify_service.PullRequestReview(ctx, pr, review, comm, mentions)

	for _, lines := range review.CodeComments {
		for _, comments := range lines {
			for _, codeComment := range comments {
				mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, codeComment.Content)
				if err != nil {
					return nil, nil, err
				}
				notify_service.PullRequestCodeComment(ctx, pr, codeComment, mentions)
			}
		}
	}

	for _, lines := range review.DiscussionComments {
		for _, comments := range lines {
			for _, discussionComment := range comments {
				mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, discussionComment.Content)
				if err != nil {
					return nil, nil, err
				}
				notify_service.PullRequestCodeComment(ctx, pr, discussionComment, mentions)
			}
		}
	}

	return review, comm, nil
}

// DismissApprovalReviews dismiss all approval reviews because of new commits
func DismissApprovalReviews(ctx context.Context, doer *user_model.User, pull *issues_model.PullRequest) error {
	reviews, err := issues_model.FindReviews(ctx, issues_model.FindReviewOptions{
		ListOptions: db.ListOptionsAll,
		IssueID:     pull.IssueID,
		Types:       []issues_model.ReviewType{issues_model.ReviewTypeApprove},
		Dismissed:   optional.Some(false),
	})
	if err != nil {
		return err
	}

	if err := reviews.LoadIssues(ctx); err != nil {
		return err
	}

	return db.WithTx(ctx, func(ctx context.Context) error {
		for _, review := range reviews {
			if err := issues_model.DismissReview(ctx, review, true); err != nil {
				return err
			}

			comment, err := issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
				Doer:     doer,
				Content:  "New commits pushed, approval review dismissed automatically according to repository settings",
				Type:     issues_model.CommentTypeDismissReview,
				ReviewID: review.ID,
				Issue:    review.Issue,
				Repo:     review.Issue.Repo,
			})
			if err != nil {
				return err
			}

			comment.Review = review
			comment.Poster = doer
			comment.Issue = review.Issue

			notify_service.PullReviewDismiss(ctx, doer, review, comment)
		}
		return nil
	})
}

// DismissReview dismissing stale review by repo admin
func DismissReview(ctx context.Context, reviewID, repoID int64, message string, doer *user_model.User, isDismiss, dismissPriors bool) (comment *issues_model.Comment, err error) {
	review, err := issues_model.GetReviewByID(ctx, reviewID)
	if err != nil {
		return nil, err
	}

	if review.Type != issues_model.ReviewTypeApprove && review.Type != issues_model.ReviewTypeReject {
		return nil, fmt.Errorf("not need to dismiss this review because it's type is not Approve or change request")
	}

	// load data for notify
	if err := review.LoadAttributes(ctx); err != nil {
		return nil, err
	}

	// Check if the review's repoID is the one we're currently expecting.
	if review.Issue.RepoID != repoID {
		return nil, fmt.Errorf("reviews's repository is not the same as the one we expect")
	}

	issue := review.Issue

	if issue.IsClosed {
		return nil, ErrDismissRequestOnClosedPR{}
	}

	if issue.IsPull {
		if err := issue.LoadPullRequest(ctx); err != nil {
			return nil, err
		}
		if issue.PullRequest.HasMerged {
			return nil, ErrDismissRequestOnClosedPR{}
		}
	}

	if err := issues_model.DismissReview(ctx, review, isDismiss); err != nil {
		return nil, err
	}

	if dismissPriors {
		reviews, err := issues_model.FindReviews(ctx, issues_model.FindReviewOptions{
			IssueID:    review.IssueID,
			ReviewerID: review.ReviewerID,
			Dismissed:  optional.Some(false),
		})
		if err != nil {
			return nil, err
		}
		for _, oldReview := range reviews {
			if err = issues_model.DismissReview(ctx, oldReview, true); err != nil {
				return nil, err
			}
		}
	}

	if !isDismiss {
		return nil, nil
	}

	if err := review.Issue.LoadAttributes(ctx); err != nil {
		return nil, err
	}

	comment, err = issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
		Doer:     doer,
		Content:  message,
		Type:     issues_model.CommentTypeDismissReview,
		ReviewID: review.ID,
		Issue:    review.Issue,
		Repo:     review.Issue.Repo,
	})
	if err != nil {
		return nil, err
	}

	comment.Review = review
	comment.Poster = doer
	comment.Issue = review.Issue

	notify_service.PullReviewDismiss(ctx, doer, review, comment)

	return comment, nil
}
// Copyright 2019 The Gitea Authors.
// All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"code.gitea.io/gitea/models/db"
	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/system"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/optional"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/gitverse"
	notify_service "code.gitea.io/gitea/services/notify"
)

var notEnoughLines = regexp.MustCompile(`fatal: file .* has only \d+ lines?`)

// ErrDismissRequestOnClosedPR represents an error when an user tries to dismiss a review associated to a closed or merged PR.
type ErrDismissRequestOnClosedPR struct{}

// IsErrDismissRequestOnClosedPR checks if an error is an ErrDismissRequestOnClosedPR.
func IsErrDismissRequestOnClosedPR(err error) bool {
	_, ok := err.(ErrDismissRequestOnClosedPR)
	return ok
}

func (err ErrDismissRequestOnClosedPR) Error() string {
	return "can't dismiss a review associated to a closed or merged PR"
}

func (err ErrDismissRequestOnClosedPR) Unwrap() error {
	return util.ErrPermissionDenied
}

// ErrSubmitReviewOnClosedPR represents an error when an user tries to submit an approve or reject review associated to a closed or merged PR.
var ErrSubmitReviewOnClosedPR = errors.New("can't submit review for a closed or merged PR")

// checkInvalidation checks if the line of code comment got changed by another commit.
// If the line got changed the comment is going to be invalidated.
func checkInvalidation(ctx context.Context, c *issues_model.Comment, repo *git.Repository, branch string) error {
	// FIXME differentiate between previous and proposed line
	commit, err := repo.LineBlame(branch, repo.Path, c.TreePath, uint(c.UnsignedLine()))
	if err != nil && (strings.Contains(err.Error(), "fatal: no such path") || notEnoughLines.MatchString(err.Error())) {
		c.Invalidated = true
		return issues_model.UpdateCommentInvalidate(ctx, c)
	}
	if err != nil {
		return err
	}
	if c.CommitSHA != "" && c.CommitSHA != commit.ID.String() {
		c.Invalidated = true
		return issues_model.UpdateCommentInvalidate(ctx, c)
	}
	return nil
}

// InvalidateCodeComments will lookup the prs for code comments which got invalidated by change
func InvalidateCodeComments(ctx context.Context, prs issues_model.PullRequestList, doer *user_model.User, repo *git.Repository, branch string) error {
	if len(prs) == 0 {
		return nil
	}
	issueIDs := prs.GetIssueIDs()

	codeComments, err := db.Find[issues_model.Comment](ctx, issues_model.FindCommentsOptions{
		ListOptions: db.ListOptionsAll,
		Type:        issues_model.CommentTypeCode,
		Invalidated: optional.Some(false),
		IssueIDs:    issueIDs,
	})
	if err != nil {
		return fmt.Errorf("find code comments: %v", err)
	}
	for _, comment := range codeComments {
		if err := checkInvalidation(ctx, comment, repo, branch); err != nil {
			return err
		}
	}
	return nil
}

// CreateCodeComment creates a comment on the code line
func CreateCodeComment(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, issue *issues_model.Issue, line int64, content, treePath string, pendingReview bool, replyReviewID int64, latestCommitID string, attachments []string) (*issues_model.Comment, error) {
	var (
		existsReview bool
		err          error
	)

	// CreateCodeComment() is used for:
	// - Single comments
	// - Comments that are part of a review
	// - Comments that reply to an existing review

	if !pendingReview && replyReviewID != 0 {
		// It's not part of a review; maybe a reply to a review comment or a single comment.
		// Check if there are reviews for that line already; if there are, this is a reply
		if existsReview, err = issues_model.ReviewExists(ctx, issue, treePath, line); err != nil {
			return nil, err
		}
	}

	// Comments that are replies don't require a review header to show up in the issue view
	if !pendingReview && existsReview {
		if err = issue.LoadRepo(ctx); err != nil {
			return nil, err
		}

		comment, err := createCodeComment(ctx,
			doer,
			issue.Repo,
			issue,
			content,
			treePath,
			line,
			replyReviewID,
			attachments,
		)
		if err != nil {
			return nil, err
		}

		mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, comment.Content)
		if err != nil {
			return nil, err
		}

		notify_service.CreateIssueComment(ctx, doer, issue.Repo, issue, comment, mentions)

		return comment, nil
	}

	review, err := issues_model.GetCurrentReview(ctx, doer, issue)
	if err != nil {
		if !issues_model.IsErrReviewNotExist(err) {
			return nil, err
		}

		if review, err = issues_model.CreateReview(ctx, issues_model.CreateReviewOptions{
			Type:     issues_model.ReviewTypePending,
			Reviewer: doer,
			Issue:    issue,
			Official: false,
			CommitID: latestCommitID,
		}); err != nil {
			return nil, err
		}
	}

	comment, err := createCodeComment(ctx,
		doer,
		issue.Repo,
		issue,
		content,
		treePath,
		line,
		review.ID,
		attachments,
	)
	comment.CommitSHA = latestCommitID
	comment.Type = issues_model.CommentTypeComment
	if err != nil {
		return nil, err
	}

	if !pendingReview && !existsReview {
		// Submit the review we've just created so the comment shows up in the issue view
		if _, _, err = SubmitReview(ctx, doer, gitRepo, issue, issues_model.ReviewTypeComment, content, latestCommitID, nil); err != nil {
			return nil, err
		}
	}

	// NOTICE: if it's a pending review the notifications will not be fired until user submit review.

	return comment, nil
}

// createCodeComment creates a plain code comment at the specified line / path
func createCodeComment(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, issue *issues_model.Issue, content, treePath string, line, reviewID int64, attachments []string) (*issues_model.Comment, error) {
	var commitID, patch string
	if err := issue.LoadPullRequest(ctx); err != nil {
		return nil, fmt.Errorf("LoadPullRequest: %w", err)
	}
	pr := issue.PullRequest
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return nil, fmt.Errorf("LoadBaseRepo: %w", err)
	}
	gitRepo, closer, err := gitrepo.RepositoryFromContextOrOpen(ctx, pr.BaseRepo)
	if err != nil {
		return nil, fmt.Errorf("RepositoryFromContextOrOpen: %w", err)
	}
	defer closer.Close()

	invalidated := false
	head := pr.GetGitRefName()
	if line > 0 {
		if reviewID != 0 {
			first, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
				ReviewID: reviewID,
				Line:     line,
				TreePath: treePath,
				Type:     issues_model.CommentTypeCode,
				ListOptions: db.ListOptions{
					PageSize: 1,
					Page:     1,
				},
			})
			if err == nil && len(first) > 0 {
				commitID = first[0].CommitSHA
				invalidated = first[0].Invalidated
				patch = first[0].Patch
			} else if err != nil && !issues_model.IsErrCommentNotExist(err) {
				return nil, fmt.Errorf("Find first comment for %d line %d path %s. Error: %w", reviewID, line, treePath, err)
			} else {
				review, err := issues_model.GetReviewByID(ctx, reviewID)
				if err == nil && len(review.CommitID) > 0 {
					head = review.CommitID
				} else if err != nil && !issues_model.IsErrReviewNotExist(err) {
					return nil, fmt.Errorf("GetReviewByID %d. Error: %w", reviewID, err)
				}
			}
		}

		if len(commitID) == 0 {
			// FIXME validate treePath
			// Get latest commit referencing the commented line
			// No need for get commit for base branch changes
			commit, err := gitRepo.LineBlame(head, gitRepo.Path, treePath, uint(line))
			if err == nil {
				commitID = commit.ID.String()
			} else if !(strings.Contains(err.Error(), "exit status 128 - fatal: no such path") || notEnoughLines.MatchString(err.Error())) {
				return nil, fmt.Errorf("LineBlame[%s, %s, %s, %d]: %w", pr.GetGitRefName(), gitRepo.Path, treePath, line, err)
			}
		}
	}

	// Only fetch diff if comment is review comment
	if len(patch) == 0 && reviewID != 0 {
		headCommitID, err := gitRepo.GetRefCommitID(pr.GetGitRefName())
		if err != nil {
			return nil, fmt.Errorf("GetRefCommitID[%s]: %w", pr.GetGitRefName(), err)
		}
		if len(commitID) == 0 {
			commitID = headCommitID
		}
		reader, writer := io.Pipe()
		defer func() {
			_ = reader.Close()
			_ = writer.Close()
		}()
		go func() {
			if err := git.GetRepoRawDiffForFile(gitRepo, pr.MergeBase, headCommitID, git.RawDiffNormal, treePath, writer); err != nil {
				_ = writer.CloseWithError(fmt.Errorf("GetRawDiffForLine[%s, %s, %s, %s]: %w", gitRepo.Path, pr.MergeBase, headCommitID, treePath, err))
				return
			}
			_ = writer.Close()
		}()

		patch, err = git.CutDiffAroundLine(reader, int64((&issues_model.Comment{Line: line}).UnsignedLine()), line < 0, setting.UI.CodeCommentLines)
		if err != nil {
			log.Error("Error whilst generating patch: %v", err)
			return nil, err
		}
	}
	return issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
		Type:        issues_model.CommentTypeCode,
		Doer:        doer,
		Repo:        repo,
		Issue:       issue,
		Content:     content,
		LineNum:     line,
		TreePath:    treePath,
		CommitSHA:   commitID,
		ReviewID:    reviewID,
		Patch:       patch,
		Invalidated: invalidated,
		Attachments: attachments,
	})
}

// SubmitReview creates a review out of the existing pending review or creates a new one if no pending review exist
func SubmitReview(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, issue *issues_model.Issue, reviewType issues_model.ReviewType, content, commitID string, attachmentUUIDs []string) (*issues_model.Review, *issues_model.Comment, error) {
	isPullRequestThreadsAvailable := gitverse.IsServiceExistForUser(ctx, system.NewDesignActivity, doer)
	if err := issue.LoadPullRequest(ctx); err != nil {
		return nil, nil, err
	}

	pr := issue.PullRequest
	var stale bool
	if reviewType != issues_model.ReviewTypeApprove && reviewType != issues_model.ReviewTypeReject {
		stale = false
	} else {
		if issue.IsClosed {
			return nil, nil, ErrSubmitReviewOnClosedPR
		}

		headCommitID, err := gitRepo.GetRefCommitID(pr.GetGitRefName())
		if err != nil {
			return nil, nil, err
		}

		if headCommitID == commitID {
			stale = false
		} else {
			stale, err = checkIfPRContentChanged(ctx, pr, commitID, headCommitID)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	review, comm, err := issues_model.SubmitReview(ctx, doer, issue, reviewType, content, commitID, stale, attachmentUUIDs, false)
	if err != nil {
		return nil, nil, err
	}
	err = issues_model.UpdateCommentsInReview(ctx, review, comm.CreatedUnix, isPullRequestThreadsAvailable)
	if err != nil {
		return nil, nil, err
	}

	mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, comm.Content)
	if err != nil {
		return nil, nil, err
	}
	notify_service.PullRequestReview(ctx, pr, review, comm, mentions)

	for _, lines := range review.CodeComments {
		for _, comments := range lines {
			for _, codeComment := range comments {
				mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, codeComment.Content)
				if err != nil {
					return nil, nil, err
				}
				notify_service.PullRequestCodeComment(ctx, pr, codeComment, mentions)
			}
		}
	}

	for _, lines := range review.DiscussionComments {
		for _, comments := range lines {
			for _, discussionComment := range comments {
				mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, discussionComment.Content)
				if err != nil {
					return nil, nil, err
				}
				notify_service.PullRequestCodeComment(ctx, pr, discussionComment, mentions)
			}
		}
	}

	return review, comm, nil
}

// DismissApprovalReviews dismiss all approval reviews because of new commits
func DismissApprovalReviews(ctx context.Context, doer *user_model.User, pull *issues_model.PullRequest) error {
	reviews, err := issues_model.FindReviews(ctx, issues_model.FindReviewOptions{
		ListOptions: db.ListOptionsAll,
		IssueID:     pull.IssueID,
		Types:       []issues_model.ReviewType{issues_model.ReviewTypeApprove},
		Dismissed:   optional.Some(false),
	})
	if err != nil {
		return err
	}

	if err := reviews.LoadIssues(ctx); err != nil {
		return err
	}

	return db.WithTx(ctx, func(ctx context.Context) error {
		for _, review := range reviews {
			if err := issues_model.DismissReview(ctx, review, true); err != nil {
				return err
			}

			comment, err := issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
				Doer:     doer,
				Content:  "New commits pushed, approval review dismissed automatically according to repository settings",
				Type:     issues_model.CommentTypeDismissReview,
				ReviewID: review.ID,
				Issue:    review.Issue,
				Repo:     review.Issue.Repo,
			})
			if err != nil {
				return err
			}

			comment.Review = review
			comment.Poster = doer
			comment.Issue = review.Issue

			notify_service.PullReviewDismiss(ctx, doer, review, comment)
		}
		return nil
	})
}

// DismissReview dismissing stale review by repo admin
func DismissReview(ctx context.Context, reviewID, repoID int64, message string, doer *user_model.User, isDismiss, dismissPriors bool) (comment *issues_model.Comment, err error) {
	review, err := issues_model.GetReviewByID(ctx, reviewID)
	if err != nil {
		return nil, err
	}

	if review.Type != issues_model.ReviewTypeApprove && review.Type != issues_model.ReviewTypeReject {
		return nil, fmt.Errorf("not need to dismiss this review because it's type is not Approve or change request")
	}

	// load data for notify
	if err := review.LoadAttributes(ctx); err != nil {
		return nil, err
	}

	// Check if the review's repoID is the one we're currently expecting.
	if review.Issue.RepoID != repoID {
		return nil, fmt.Errorf("reviews's repository is not the same as the one we expect")
	}

	issue := review.Issue

	if issue.IsClosed {
		return nil, ErrDismissRequestOnClosedPR{}
	}

	if issue.IsPull {
		if err := issue.LoadPullRequest(ctx); err != nil {
			return nil, err
		}
		if issue.PullRequest.HasMerged {
			return nil, ErrDismissRequestOnClosedPR{}
		}
	}

	if err := issues_model.DismissReview(ctx, review, isDismiss); err != nil {
		return nil, err
	}

	if dismissPriors {
		reviews, err := issues_model.FindReviews(ctx, issues_model.FindReviewOptions{
			IssueID:    review.IssueID,
			ReviewerID: review.ReviewerID,
			Dismissed:  optional.Some(false),
		})
		if err != nil {
			return nil, err
		}
		for _, oldReview := range reviews {
			if err = issues_model.DismissReview(ctx, oldReview, true); err != nil {
				return nil, err
			}
		}
	}

	if !isDismiss {
		return nil, nil
	}

	if err := review.Issue.LoadAttributes(ctx); err != nil {
		return nil, err
	}

	comment, err = issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
		Doer:     doer,
		Content:  message,
		Type:     issues_model.CommentTypeDismissReview,
		ReviewID: review.ID,
		Issue:    review.Issue,
		Repo:     review.Issue.Repo,
	})
	if err != nil {
		return nil, err
	}

	comment.Review = review
	comment.Poster = doer
	comment.Issue = review.Issue

	notify_service.PullReviewDismiss(ctx, doer, review, comment)

	return comment, nil
}
// Copyright 2019 The Gitea Authors.
// All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"code.gitea.io/gitea/models/db"
	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/system"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/optional"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/gitverse"
	notify_service "code.gitea.io/gitea/services/notify"
)

var notEnoughLines = regexp.MustCompile(`fatal: file .* has only \d+ lines?`)

// ErrDismissRequestOnClosedPR represents an error when an user tries to dismiss a review associated to a closed or merged PR.
type ErrDismissRequestOnClosedPR struct{}

// IsErrDismissRequestOnClosedPR checks if an error is an ErrDismissRequestOnClosedPR.
func IsErrDismissRequestOnClosedPR(err error) bool {
	_, ok := err.(ErrDismissRequestOnClosedPR)
	return ok
}

func (err ErrDismissRequestOnClosedPR) Error() string {
	return "can't dismiss a review associated to a closed or merged PR"
}

func (err ErrDismissRequestOnClosedPR) Unwrap() error {
	return util.ErrPermissionDenied
}

// ErrSubmitReviewOnClosedPR represents an error when an user tries to submit an approve or reject review associated to a closed or merged PR.
var ErrSubmitReviewOnClosedPR = errors.New("can't submit review for a closed or merged PR")

// checkInvalidation checks if the line of code comment got changed by another commit.
// If the line got changed the comment is going to be invalidated.
func checkInvalidation(ctx context.Context, c *issues_model.Comment, repo *git.Repository, branch string) error {
	// FIXME differentiate between previous and proposed line
	commit, err := repo.LineBlame(branch, repo.Path, c.TreePath, uint(c.UnsignedLine()))
	if err != nil && (strings.Contains(err.Error(), "fatal: no such path") || notEnoughLines.MatchString(err.Error())) {
		c.Invalidated = true
		return issues_model.UpdateCommentInvalidate(ctx, c)
	}
	if err != nil {
		return err
	}
	if c.CommitSHA != "" && c.CommitSHA != commit.ID.String() {
		c.Invalidated = true
		return issues_model.UpdateCommentInvalidate(ctx, c)
	}
	return nil
}

// InvalidateCodeComments will lookup the prs for code comments which got invalidated by change
func InvalidateCodeComments(ctx context.Context, prs issues_model.PullRequestList, doer *user_model.User, repo *git.Repository, branch string) error {
	if len(prs) == 0 {
		return nil
	}
	issueIDs := prs.GetIssueIDs()

	codeComments, err := db.Find[issues_model.Comment](ctx, issues_model.FindCommentsOptions{
		ListOptions: db.ListOptionsAll,
		Type:        issues_model.CommentTypeCode,
		Invalidated: optional.Some(false),
		IssueIDs:    issueIDs,
	})
	if err != nil {
		return fmt.Errorf("find code comments: %v", err)
	}
	for _, comment := range codeComments {
		if err := checkInvalidation(ctx, comment, repo, branch); err != nil {
			return err
		}
	}
	return nil
}

// CreateCodeComment creates a comment on the code line
func CreateCodeComment(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, issue *issues_model.Issue, line int64, content, treePath string, pendingReview bool, replyReviewID int64, latestCommitID string, attachments []string) (*issues_model.Comment, error) {
	var (
		existsReview bool
		err          error
	)

	// CreateCodeComment() is used for:
	// - Single comments
	// - Comments that are part of a review
	// - Comments that reply to an existing review

	if !pendingReview && replyReviewID != 0 {
		// It's not part of a review; maybe a reply to a review comment or a single comment.
		// Check if there are reviews for that line already; if there are, this is a reply
		if existsReview, err = issues_model.ReviewExists(ctx, issue, treePath, line); err != nil {
			return nil, err
		}
	}

	// Comments that are replies don't require a review header to show up in the issue view
	if !pendingReview && existsReview {
		if err = issue.LoadRepo(ctx); err != nil {
			return nil, err
		}

		comment, err := createCodeComment(ctx,
			doer,
			issue.Repo,
			issue,
			content,
			treePath,
			line,
			replyReviewID,
			attachments,
		)
		if err != nil {
			return nil, err
		}

		mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, comment.Content)
		if err != nil {
			return nil, err
		}

		notify_service.CreateIssueComment(ctx, doer, issue.Repo, issue, comment, mentions)

		return comment, nil
	}

	review, err := issues_model.GetCurrentReview(ctx, doer, issue)
	if err != nil {
		if !issues_model.IsErrReviewNotExist(err) {
			return nil, err
		}

		if review, err = issues_model.CreateReview(ctx, issues_model.CreateReviewOptions{
			Type:     issues_model.ReviewTypePending,
			Reviewer: doer,
			Issue:    issue,
			Official: false,
			CommitID: latestCommitID,
		}); err != nil {
			return nil, err
		}
	}

	comment, err := createCodeComment(ctx,
		doer,
		issue.Repo,
		issue,
		content,
		treePath,
		line,
		review.ID,
		attachments,
	)
	comment.CommitSHA = latestCommitID
	comment.Type = issues_model.CommentTypeComment
	if err != nil {
		return nil, err
	}

	if !pendingReview && !existsReview {
		// Submit the review we've just created so the comment shows up in the issue view
		if _, _, err = SubmitReview(ctx, doer, gitRepo, issue, issues_model.ReviewTypeComment, content, latestCommitID, nil); err != nil {
			return nil, err
		}
	}

	// NOTICE: if it's a pending review the notifications will not be fired until user submit review.

	return comment, nil
}

// createCodeComment creates a plain code comment at the specified line / path
func createCodeComment(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, issue *issues_model.Issue, content, treePath string, line, reviewID int64, attachments []string) (*issues_model.Comment, error) {
	var commitID, patch string
	if err := issue.LoadPullRequest(ctx); err != nil {
		return nil, fmt.Errorf("LoadPullRequest: %w", err)
	}
	pr := issue.PullRequest
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return nil, fmt.Errorf("LoadBaseRepo: %w", err)
	}
	gitRepo, closer, err := gitrepo.RepositoryFromContextOrOpen(ctx, pr.BaseRepo)
	if err != nil {
		return nil, fmt.Errorf("RepositoryFromContextOrOpen: %w", err)
	}
	defer closer.Close()

	invalidated := false
	head := pr.GetGitRefName()
	if line > 0 {
		if reviewID != 0 {
			first, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
				ReviewID: reviewID,
				Line:     line,
				TreePath: treePath,
				Type:     issues_model.CommentTypeCode,
				ListOptions: db.ListOptions{
					PageSize: 1,
					Page:     1,
				},
			})
			if err == nil && len(first) > 0 {
				commitID = first[0].CommitSHA
				invalidated = first[0].Invalidated
				patch = first[0].Patch
			} else if err != nil && !issues_model.IsErrCommentNotExist(err) {
				return nil, fmt.Errorf("Find first comment for %d line %d path %s. Error: %w", reviewID, line, treePath, err)
			} else {
				review, err := issues_model.GetReviewByID(ctx, reviewID)
				if err == nil && len(review.CommitID) > 0 {
					head = review.CommitID
				} else if err != nil && !issues_model.IsErrReviewNotExist(err) {
					return nil, fmt.Errorf("GetReviewByID %d. Error: %w", reviewID, err)
				}
			}
		}

		if len(commitID) == 0 {
			// FIXME validate treePath
			// Get latest commit referencing the commented line
			// No need for get commit for base branch changes
			commit, err := gitRepo.LineBlame(head, gitRepo.Path, treePath, uint(line))
			if err == nil {
				commitID = commit.ID.String()
			} else if !(strings.Contains(err.Error(), "exit status 128 - fatal: no such path") || notEnoughLines.MatchString(err.Error())) {
				return nil, fmt.Errorf("LineBlame[%s, %s, %s, %d]: %w", pr.GetGitRefName(), gitRepo.Path, treePath, line, err)
			}
		}
	}

	// Only fetch diff if comment is review comment
	if len(patch) == 0 && reviewID != 0 {
		headCommitID, err := gitRepo.GetRefCommitID(pr.GetGitRefName())
		if err != nil {
			return nil, fmt.Errorf("GetRefCommitID[%s]: %w", pr.GetGitRefName(), err)
		}
		if len(commitID) == 0 {
			commitID = headCommitID
		}
		reader, writer := io.Pipe()
		defer func() {
			_ = reader.Close()
			_ = writer.Close()
		}()
		go func() {
			if err := git.GetRepoRawDiffForFile(gitRepo, pr.MergeBase, headCommitID, git.RawDiffNormal, treePath, writer); err != nil {
				_ = writer.CloseWithError(fmt.Errorf("GetRawDiffForLine[%s, %s, %s, %s]: %w", gitRepo.Path, pr.MergeBase, headCommitID, treePath, err))
				return
			}
			_ = writer.Close()
		}()

		patch, err = git.CutDiffAroundLine(reader, int64((&issues_model.Comment{Line: line}).UnsignedLine()), line < 0, setting.UI.CodeCommentLines)
		if err != nil {
			log.Error("Error whilst generating patch: %v", err)
			return nil, err
		}
	}
	return issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
		Type:        issues_model.CommentTypeCode,
		Doer:        doer,
		Repo:        repo,
		Issue:       issue,
		Content:     content,
		LineNum:     line,
		TreePath:    treePath,
		CommitSHA:   commitID,
		ReviewID:    reviewID,
		Patch:       patch,
		Invalidated: invalidated,
		Attachments: attachments,
	})
}

// SubmitReview creates a review out of the existing pending review or creates a new one if no pending review exist
func SubmitReview(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, issue *issues_model.Issue, reviewType issues_model.ReviewType, content, commitID string, attachmentUUIDs []string) (*issues_model.Review, *issues_model.Comment, error) {
	isPullRequestThreadsAvailable := gitverse.IsServiceExistForUser(ctx, system.NewDesignActivity, doer)
	if err := issue.LoadPullRequest(ctx); err != nil {
		return nil, nil, err
	}

	pr := issue.PullRequest
	var stale bool
	if reviewType != issues_model.ReviewTypeApprove && reviewType != issues_model.ReviewTypeReject {
		stale = false
	} else {
		if issue.IsClosed {
			return nil, nil, ErrSubmitReviewOnClosedPR
		}

		headCommitID, err := gitRepo.GetRefCommitID(pr.GetGitRefName())
		if err != nil {
			return nil, nil, err
		}

		if headCommitID == commitID {
			stale = false
		} else {
			stale, err = checkIfPRContentChanged(ctx, pr, commitID, headCommitID)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	review, comm, err := issues_model.SubmitReview(ctx, doer, issue, reviewType, content, commitID, stale, attachmentUUIDs, false)
	if err != nil {
		return nil, nil, err
	}
	err = issues_model.UpdateCommentsInReview(ctx, review, comm.CreatedUnix, isPullRequestThreadsAvailable)
	if err != nil {
		return nil, nil, err
	}

	mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, comm.Content)
	if err != nil {
		return nil, nil, err
	}
	notify_service.PullRequestReview(ctx, pr, review, comm, mentions)

	for _, lines := range review.CodeComments {
		for _, comments := range lines {
			for _, codeComment := range comments {
				mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, codeComment.Content)
				if err != nil {
					return nil, nil, err
				}
				notify_service.PullRequestCodeComment(ctx, pr, codeComment, mentions)
			}
		}
	}

	for _, lines := range review.DiscussionComments {
		for _, comments := range lines {
			for _, discussionComment := range comments {
				mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, discussionComment.Content)
				if err != nil {
					return nil, nil, err
				}
				notify_service.PullRequestCodeComment(ctx, pr, discussionComment, mentions)
			}
		}
	}

	return review, comm, nil
}

// DismissApprovalReviews dismiss all approval reviews because of new commits
func DismissApprovalReviews(ctx context.Context, doer *user_model.User, pull *issues_model.PullRequest) error {
	reviews, err := issues_model.FindReviews(ctx, issues_model.FindReviewOptions{
		ListOptions: db.ListOptionsAll,
		IssueID:     pull.IssueID,
		Types:       []issues_model.ReviewType{issues_model.ReviewTypeApprove},
		Dismissed:   optional.Some(false),
	})
	if err != nil {
		return err
	}

	if err := reviews.LoadIssues(ctx); err != nil {
		return err
	}

	return db.WithTx(ctx, func(ctx context.Context) error {
		for _, review := range reviews {
			if err := issues_model.DismissReview(ctx, review, true); err != nil {
				return err
			}

			comment, err := issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
				Doer:     doer,
				Content:  "New commits pushed, approval review dismissed automatically according to repository settings",
				Type:     issues_model.CommentTypeDismissReview,
				ReviewID: review.ID,
				Issue:    review.Issue,
				Repo:     review.Issue.Repo,
			})
			if err != nil {
				return err
			}

			comment.Review = review
			comment.Poster = doer
			comment.Issue = review.Issue

			notify_service.PullReviewDismiss(ctx, doer, review, comment)
		}
		return nil
	})
}

// DismissReview dismissing stale review by repo admin
func DismissReview(ctx context.Context, reviewID, repoID int64, message string, doer *user_model.User, isDismiss, dismissPriors bool) (comment *issues_model.Comment, err error) {
	review, err := issues_model.GetReviewByID(ctx, reviewID)
	if err != nil {
		return nil, err
	}

	if review.Type != issues_model.ReviewTypeApprove && review.Type != issues_model.ReviewTypeReject {
		return nil, fmt.Errorf("not need to dismiss this review because it's type is not Approve or change request")
	}

	// load data for notify
	if err := review.LoadAttributes(ctx); err != nil {
		return nil, err
	}

	// Check if the review's repoID is the one we're currently expecting.
	if review.Issue.RepoID != repoID {
		return nil, fmt.Errorf("reviews's repository is not the same as the one we expect")
	}

	issue := review.Issue

	if issue.IsClosed {
		return nil, ErrDismissRequestOnClosedPR{}
	}

	if issue.IsPull {
		if err := issue.LoadPullRequest(ctx); err != nil {
			return nil, err
		}
		if issue.PullRequest.HasMerged {
			return nil, ErrDismissRequestOnClosedPR{}
		}
	}

	if err := issues_model.DismissReview(ctx, review, isDismiss); err != nil {
		return nil, err
	}

	if dismissPriors {
		reviews, err := issues_model.FindReviews(ctx, issues_model.FindReviewOptions{
			IssueID:    review.IssueID,
			ReviewerID: review.ReviewerID,
			Dismissed:  optional.Some(false),
		})
		if err != nil {
			return nil, err
		}
		for _, oldReview := range reviews {
			if err = issues_model.DismissReview(ctx, oldReview, true); err != nil {
				return nil, err
			}
		}
	}

	if !isDismiss {
		return nil, nil
	}

	if err := review.Issue.LoadAttributes(ctx); err != nil {
		return nil, err
	}

	comment, err = issues_model.CreateComment(ctx, &issues_model.CreateCommentOptions{
		Doer:     doer,
		Content:  message,
		Type:     issues_model.CommentTypeDismissReview,
		ReviewID: review.ID,
		Issue:    review.Issue,
		Repo:     review.Issue.Repo,
	})
	if err != nil {
		return nil, err
	}

	comment.Review = review
	comment.Poster = doer
	comment.Issue = review.Issue

	notify_service.PullReviewDismiss(ctx, doer, review, comment)

	return comment, nil
}

	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	affectedFiles := make([]string, 0, 32)

	// Run `git diff --name-only` to get the names of the changed files
	err = NewCommand(repo.Ctx, "diff", "--name-only").AddDynamicArguments(oldCommitID, newCommitID).
		Run(&RunOpts{
			Env:    env,
			Dir:    repo.Path,
			Stdout: stdoutWriter,
			PipelineFunc: func(ctx context.Context, cancel context.CancelFunc) error {
				// Close the writer end of the pipe to begin processing
				_ = stdoutWriter.Close()
				defer func() {
					// Close the reader on return to terminate the git command if necessary
					_ = stdoutReader.Close()
				}()
				// Now scan the output from the command
				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					affectedFiles = append(affectedFiles, path)
				}
				return scanner.Err()
			},
		})
	if err != nil {
		log.Error("Unable to get affected files for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}

	return affectedFiles, err
}
