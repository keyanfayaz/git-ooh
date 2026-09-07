package service_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keyanfayaz/git-ooh/internal/model"
	"github.com/keyanfayaz/git-ooh/internal/service"
)

func TestParallelPathReviewIntegrationAndUndo(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	t.Setenv("OOH_DATA_HOME", t.TempDir())

	svc, err := service.Open(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	detail, err := svc.CreateTask(ctx, repo, "Improve greeting", "Let two agents explore wording", "main")
	if err != nil {
		t.Fatal(err)
	}
	primary := detail.Paths[0]
	writeFile(t, filepath.Join(primary.WorktreePath, "greeting.txt"), "hello from primary\n")
	writeFile(t, filepath.Join(primary.WorktreePath, "evidence.txt"), "manual review passed\n")

	first, err := svc.CreateCheckpoint(ctx, primary, service.CheckpointInput{
		Name:               "First result",
		Summary:            "Adds a friendly greeting",
		VerificationStatus: "passed",
		VerificationNotes:  "Read the generated text",
		Ready:              true,
		ArtifactPaths:      []string{"evidence.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.CommitOID == "" || !first.Ready {
		t.Fatalf("unexpected checkpoint: %#v", first)
	}
	if _, err := svc.AddComment(ctx, service.CommentInput{CheckpointID: first.ID, FilePath: "greeting.txt", Side: "new", StartLine: 1, Body: "Try a warmer version."}); err != nil {
		t.Fatal(err)
	}

	detail, err = svc.ForkPath(ctx, primary, []string{"Warm", "Concise"}, "Compare tone")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Paths) != 3 {
		t.Fatalf("got %d paths, want 3", len(detail.Paths))
	}
	warm := pathNamed(t, detail.Paths, "Warm")
	writeFile(t, filepath.Join(warm.WorktreePath, "greeting.txt"), "hello — glad you are here\n")
	warmCheckpoint, err := svc.CreateCheckpoint(ctx, warm, service.CheckpointInput{
		Name:               "Warm result",
		Summary:            "Uses a warmer greeting",
		VerificationStatus: "passed",
		VerificationNotes:  "Compared both variants",
		Ready:              true,
	})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := svc.CheckpointDiff(ctx, warmCheckpoint.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Patch, "glad you are here") {
		t.Fatalf("checkpoint diff did not contain the selected change:\n%s", diff.Patch)
	}

	detail, err = svc.SelectCheckpoint(ctx, warmCheckpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.SelectedCheckpointID != warmCheckpoint.ID || detail.Task.State != model.TaskReady {
		t.Fatalf("selection was not recorded: %#v", detail.Task)
	}
	for _, path := range detail.Paths {
		if path.ID != warm.ID && path.State != model.PathArchived {
			t.Fatalf("unselected path %s was not archived: %s", path.Name, path.State)
		}
	}

	candidate, err := svc.PrepareIntegration(ctx, detail.Task.ID, warmCheckpoint.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != model.CandidateReady || candidate.CommitOID == "" {
		t.Fatalf("candidate not ready: %#v", candidate)
	}

	dirtyBase := filepath.Join(repo, "local-note.txt")
	writeFile(t, dirtyBase, "do not disturb\n")
	if _, err := svc.ApplyIntegration(ctx, candidate.ID, candidate.CommitOID); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("dirty base should safely block integration, got %v", err)
	}
	warmAfterBlock, err := svc.Store.GetPath(ctx, warm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if warmAfterBlock.State == model.PathArchived {
		t.Fatal("integration preflight archived the selected path before detecting the dirty base")
	}
	if err := os.Remove(dirtyBase); err != nil {
		t.Fatal(err)
	}

	integration, err := svc.ApplyIntegration(ctx, candidate.ID, candidate.CommitOID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "greeting.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello — glad you are here\n" {
		t.Fatalf("integrated content = %q", data)
	}

	if _, err := svc.UndoIntegration(ctx, integration.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "greeting.txt")); !os.IsNotExist(err) {
		t.Fatalf("undo should restore the original base, stat error: %v", err)
	}
	restored, err := svc.Store.GetPath(ctx, warm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.State == model.PathArchived || restored.WorktreePath == "" {
		t.Fatalf("undo did not restore the selected workspace: %#v", restored)
	}
}

func TestDraftTaskRunsAgentInIndependentCloneAndCreatesReadyCheckpoint(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	t.Setenv("OOH_DATA_HOME", t.TempDir())
	bin := t.TempDir()
	fake := filepath.Join(bin, "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "codex-cli test"
  exit 0
fi
last=""
previous=""
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then last="$argument"; fi
  previous="$argument"
done
printf 'agent-made-change\n' > agent.txt
printf '{"thread_id":"session-test","result":"done"}\n'
printf '{"summary":"Adds the agent result","verificationStatus":"passed","verificationNotes":"Fake check passed","riskNotes":""}' > "$last"
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	svc, err := service.Open(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	draft, err := svc.CreateDraftTask(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Task.State != model.TaskDraft || draft.Task.Title != "Untitled task" {
		t.Fatalf("unexpected draft: %#v", draft.Task)
	}
	clone := draft.Paths[0].WorktreePath
	mainCommon := strings.TrimSpace(runGit(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	cloneCommon := strings.TrimSpace(runGit(t, clone, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	if filepath.Clean(mainCommon) == filepath.Clean(cloneCommon) {
		t.Fatal("draft workspace should have an independent Git directory")
	}
	if remotes := strings.TrimSpace(runGit(t, clone, "remote")); remotes != "" {
		t.Fatalf("isolated clone should not retain a remote, got %q", remotes)
	}
	linked, err := service.Open(ctx, clone)
	if err != nil {
		t.Fatalf("open Ooh from isolated clone: %v", err)
	}
	linkedRoot, _ := filepath.EvalSymlinks(linked.Repo.Root)
	expectedRoot, _ := filepath.EvalSymlinks(repo)
	if linkedRoot != expectedRoot {
		t.Fatalf("isolated clone linked to %q, want %q", linked.Repo.Root, repo)
	}
	current, err := linked.CurrentPath(ctx, clone, "")
	linked.Close()
	if err != nil || current.ID != draft.Paths[0].ID {
		t.Fatalf("current path from clone = %#v, %v", current, err)
	}

	detail, err := svc.StartTask(ctx, draft.Task.ID, service.StartTaskInput{Instructions: "Add an agent result file", Variations: []service.VariationInput{{Name: "Primary", Provider: "codex", Model: "gpt-test"}, {Name: "Alternative", Provider: "codex", Guidance: "Try a separate variation."}}})
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Title != "Add an agent result file" {
		t.Fatalf("derived title = %q", detail.Task.Title)
	}
	if len(detail.Paths) != 2 {
		t.Fatalf("variation paths = %d, want 2", len(detail.Paths))
	}
	secondCommon := strings.TrimSpace(runGit(t, detail.Paths[1].WorktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	if filepath.Clean(secondCommon) == filepath.Clean(cloneCommon) || filepath.Clean(secondCommon) == filepath.Clean(mainCommon) {
		t.Fatal("each variation should have its own Git directory")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		detail, err = svc.TaskDetail(ctx, draft.Task.ID)
		if err != nil {
			t.Fatal(err)
		}
		finished := len(detail.Runs) == 2
		for _, run := range detail.Runs {
			finished = finished && run.State != model.RunQueued && run.State != model.RunRunning
		}
		if finished {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(detail.Runs) != 2 || detail.Runs[0].State != model.RunSucceeded || detail.Runs[1].State != model.RunSucceeded {
		t.Fatalf("agent run did not succeed: %#v", detail.Runs)
	}
	if detail.Runs[0].Model != "gpt-test" || detail.Runs[1].Model != "" {
		t.Fatalf("agent models were not preserved: %#v", detail.Runs)
	}
	if detail.Runs[0].ProviderSessionID != "session-test" || detail.Runs[1].ProviderSessionID != "session-test" {
		t.Fatalf("provider session was not captured: %#v", detail.Runs[0])
	}
	if len(detail.Checkpoints) != 2 || !detail.Checkpoints[0].Ready || !detail.Checkpoints[1].Ready {
		t.Fatalf("ready checkpoint was not created: %#v", detail.Checkpoints)
	}
	diff, err := svc.CheckpointDiff(ctx, detail.Checkpoints[0].ID, "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Patch, "agent-made-change") {
		t.Fatalf("isolated clone commit was not imported:\n%s", diff.Patch)
	}
}

func TestWorkspaceFileSaveUsesRevisionAndKeepsChangedDraft(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	t.Setenv("OOH_DATA_HOME", t.TempDir())
	svc, err := service.Open(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	draft, err := svc.CreateDraftTask(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	path := draft.Paths[0]
	file, err := svc.ReadWorkspaceFile(ctx, path.ID, "README.md")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.WriteWorkspaceFile(ctx, path.ID, "README.md", "# edited in browser\n", file.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Content != "# edited in browser\n" || updated.Revision == file.Revision {
		t.Fatalf("unexpected saved file: %#v", updated)
	}
	if _, err := svc.WriteWorkspaceFile(ctx, path.ID, "README.md", "stale", file.Revision); !errors.Is(err, service.ErrFileConflict) {
		t.Fatalf("stale save error = %v", err)
	}
	if err := svc.DiscardDraftTask(ctx, draft.Task.ID); err == nil || !strings.Contains(err.Error(), "kept") {
		t.Fatalf("changed draft should be kept, got %v", err)
	}
}

func newRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Ooh Test")
	runGit(t, dir, "config", "user.email", "ooh@example.test")
	writeFile(t, filepath.Join(dir, "README.md"), "# test\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial")
	return dir
}

func pathNamed(t *testing.T, paths []model.Path, name string) model.Path {
	t.Helper()
	for _, path := range paths {
		if path.Name == name {
			return path
		}
	}
	t.Fatalf("path %q not found", name)
	return model.Path{}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
