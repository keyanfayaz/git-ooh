package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/keyanfayaz/git-ooh/internal/id"
	"github.com/keyanfayaz/git-ooh/internal/model"
	"github.com/keyanfayaz/git-ooh/internal/store"
)

type AgentInfo struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Available bool         `json:"available"`
	Version   string       `json:"version,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	Models    []AgentModel `json:"models"`
}

type AgentModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type VariationInput struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Guidance string `json:"guidance"`
}

type StartTaskInput struct {
	Instructions string           `json:"instructions"`
	Variations   []VariationInput `json:"variations"`
}

type AddVariationsInput struct {
	Prompt     string           `json:"prompt"`
	Variations []VariationInput `json:"variations"`
}

type AgentOutcome struct {
	Summary            string `json:"summary"`
	VerificationStatus string `json:"verificationStatus"`
	VerificationNotes  string `json:"verificationNotes"`
	RiskNotes          string `json:"riskNotes"`
}

type RunManager struct {
	svc    *Service
	mu     sync.Mutex
	active map[string]*exec.Cmd
	closed bool
	wg     sync.WaitGroup
}

func NewRunManager(svc *Service) *RunManager {
	return &RunManager{svc: svc, active: map[string]*exec.Cmd{}}
}

func (m *RunManager) Close() {
	m.mu.Lock()
	m.closed = true
	commands := make([]*exec.Cmd, 0, len(m.active))
	for _, command := range m.active {
		commands = append(commands, command)
	}
	m.mu.Unlock()
	for _, command := range commands {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}
	m.wg.Wait()
}

func (m *RunManager) Stop(runID string) error {
	m.mu.Lock()
	command := m.active[runID]
	m.mu.Unlock()
	if command == nil || command.Process == nil {
		return errors.New("this agent run is not active")
	}
	return command.Process.Kill()
}

func AvailableAgents(ctx context.Context) []AgentInfo {
	codex := detectAgent(ctx, "codex", "Codex")
	codex.Models = agentModels("codex", configuredCodexModel())
	claude := detectAgent(ctx, "claude", "Claude Code")
	claude.Models = agentModels("claude", "")
	return []AgentInfo{codex, claude}
}

func agentModels(provider, configured string) []AgentModel {
	models := []AgentModel{{ID: "", Name: "Default"}}
	ids := []string{}
	if provider == "codex" {
		ids = append(ids, configured)
		ids = append(ids, strings.Split(os.Getenv("OOH_CODEX_MODELS"), ",")...)
	} else {
		ids = append(ids, "sonnet", "opus", "haiku")
		ids = append(ids, strings.Split(os.Getenv("OOH_CLAUDE_MODELS"), ",")...)
	}
	seen := map[string]bool{"": true}
	for _, value := range ids {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		models = append(models, AgentModel{ID: value, Name: value})
	}
	return models
}

func configuredCodexModel() string {
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(userHome, ".codex")
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return ""
	}
	match := regexp.MustCompile(`(?m)^\s*model\s*=\s*["']([^"']+)["']`).FindSubmatch(data)
	if len(match) != 2 {
		return ""
	}
	return strings.TrimSpace(string(match[1]))
}

func detectAgent(ctx context.Context, executable, name string) AgentInfo {
	info := AgentInfo{ID: executable, Name: name}
	path, err := exec.LookPath(executable)
	if err != nil {
		info.Reason = name + " is not installed"
		return info
	}
	command := exec.CommandContext(ctx, path, "--version")
	output, err := command.CombinedOutput()
	if err != nil {
		info.Reason = name + " could not be started"
		return info
	}
	info.Available = true
	info.Version = strings.TrimSpace(string(output))
	return info
}

func (s *Service) CreateDraftTask(ctx context.Context, dir string) (model.TaskDetail, error) {
	base, err := s.inferBase(ctx, dir, "")
	if err != nil {
		return model.TaskDetail{}, err
	}
	baseRef := "refs/heads/" + base
	baseOID, err := s.Git.Resolve(ctx, s.Repo, "HEAD")
	if err != nil {
		return model.TaskDetail{}, err
	}
	now := time.Now().UTC()
	taskID, pathID := id.New("task"), id.New("path")
	branch := fmt.Sprintf("ooh/%s/%s-untitled", id.Short(taskID), id.Short(pathID))
	workspace := filepath.Join(s.WorkspaceRoot, id.Short(taskID), id.Short(pathID)+"-untitled")
	task := model.Task{ID: taskID, Title: "Untitled task", BaseRef: baseRef, BaseOID: baseOID, BaseTipOID: baseOID, State: model.TaskDraft, SelectedPathID: pathID, CreatedAt: now, UpdatedAt: now}
	path := model.Path{ID: pathID, TaskID: taskID, Name: "Approach 1", BranchRef: "refs/heads/" + branch, WorktreePath: workspace, State: model.PathWorking, CreatedAt: now, UpdatedAt: now}
	err = s.withLock(func() error {
		if err := s.createPathWorkspace(ctx, workspace, path.BranchRef, baseOID, pathID); err != nil {
			return err
		}
		if err := s.Store.CreateTaskWithPath(ctx, task, path); err != nil {
			_ = s.removePathWorkspace(workspace)
			_ = s.Git.DeleteBranch(ctx, s.Repo, path.BranchRef)
			return err
		}
		return s.Store.Audit(ctx, "task.draft_created", taskID, "")
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	s.Bus.Publish("task.created", taskID)
	return s.TaskDetail(ctx, taskID)
}

func (s *Service) DiscardDraftTask(ctx context.Context, taskID string) error {
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.State != model.TaskDraft {
		return errors.New("only an untouched draft can be discarded")
	}
	runs, err := s.Store.ListAgentRuns(ctx, taskID)
	if err != nil || len(runs) != 0 {
		return errors.New("this draft has already started work")
	}
	paths, err := s.Store.ListPaths(ctx, taskID)
	if err != nil {
		return err
	}
	return s.withLock(func() error {
		for _, path := range paths {
			status, statusErr := s.Git.Status(ctx, path.WorktreePath)
			if statusErr != nil || status.Dirty {
				return errors.New("this draft contains file changes and was kept")
			}
			if err := s.removePathWorkspace(path.WorktreePath); err != nil {
				return err
			}
			_ = s.Git.DeleteBranch(ctx, s.Repo, path.BranchRef)
		}
		return s.Store.DeleteTask(ctx, taskID)
	})
}

func (s *Service) CleanupDrafts(ctx context.Context, before time.Time) ([]string, error) {
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, task := range tasks {
		if task.State != model.TaskDraft || !task.CreatedAt.Before(before) {
			continue
		}
		if s.DiscardDraftTask(ctx, task.ID) == nil {
			removed = append(removed, task.ID)
		}
	}
	return removed, nil
}

func (s *Service) StartTask(ctx context.Context, taskID string, input StartTaskInput) (model.TaskDetail, error) {
	instructions := strings.TrimSpace(input.Instructions)
	if instructions == "" {
		return model.TaskDetail{}, errors.New("describe what you want to change")
	}
	if len(input.Variations) == 0 {
		input.Variations = []VariationInput{{Name: "Approach 1", Provider: firstAvailableAgent(ctx)}}
	}
	for _, variation := range input.Variations {
		if !agentAvailable(ctx, variation.Provider) {
			return model.TaskDetail{}, fmt.Errorf("%s is not available", variation.Provider)
		}
		if err := validateAgentModel(variation.Model); err != nil {
			return model.TaskDetail{}, err
		}
	}
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if task.State != model.TaskDraft {
		return model.TaskDetail{}, errors.New("this task has already started")
	}
	paths, err := s.Store.ListPaths(ctx, taskID)
	if err != nil || len(paths) == 0 {
		return model.TaskDetail{}, errors.New("task workspace is missing")
	}
	task.Title = titleFromInstructions(instructions)
	task.Description = instructions
	task.State = model.TaskWorking
	if err := s.Store.UpdateTask(ctx, task); err != nil {
		return model.TaskDetail{}, err
	}
	primary := paths[0]
	primary.Name = variationName(input.Variations[0].Name, 1)
	if err := s.Store.UpdatePath(ctx, primary); err != nil {
		return model.TaskDetail{}, err
	}
	paths = []model.Path{primary}
	for index := 1; index < len(input.Variations); index++ {
		pathID := id.New("path")
		name := variationName(input.Variations[index].Name, index+1)
		branch := fmt.Sprintf("ooh/%s/%s-%s", id.Short(taskID), id.Short(pathID), id.Slug(name))
		workspace := filepath.Join(s.WorkspaceRoot, id.Short(taskID), id.Short(pathID)+"-"+id.Slug(name))
		path := model.Path{ID: pathID, TaskID: taskID, Name: name, BranchRef: "refs/heads/" + branch, WorktreePath: workspace, State: model.PathWorking, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		if err := s.createPathWorkspace(ctx, workspace, path.BranchRef, task.BaseOID, pathID); err != nil {
			return model.TaskDetail{}, err
		}
		if err := s.Store.CreatePath(ctx, path); err != nil {
			_ = s.removePathWorkspace(workspace)
			_ = s.Git.DeleteBranch(ctx, s.Repo, path.BranchRef)
			return model.TaskDetail{}, err
		}
		paths = append(paths, path)
	}
	for index, path := range paths {
		variation := input.Variations[index]
		prompt := instructions
		if guidance := strings.TrimSpace(variation.Guidance); guidance != "" {
			prompt += "\n\nApproach-specific direction:\n" + guidance
		}
		run := model.AgentRun{ID: id.New("run"), TaskID: taskID, PathID: path.ID, Provider: variation.Provider, Model: strings.TrimSpace(variation.Model), Prompt: prompt, State: model.RunQueued, CreatedAt: time.Now().UTC()}
		if err := s.Store.CreateAgentRun(ctx, run); err != nil {
			return model.TaskDetail{}, err
		}
		s.Runs.Start(run, path, false)
	}
	s.Bus.Publish("task.started", taskID)
	return s.TaskDetail(ctx, taskID)
}

func (s *Service) RenameTask(ctx context.Context, taskID, title string) (model.Task, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return model.Task{}, errors.New("task name cannot be empty")
	}
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return model.Task{}, err
	}
	task.Title = title
	if err := s.Store.UpdateTask(ctx, task); err != nil {
		return model.Task{}, err
	}
	s.Bus.Publish("task.updated", taskID)
	return task, nil
}

func (s *Service) ContinuePath(ctx context.Context, pathID, provider, prompt string) (model.AgentRun, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return model.AgentRun{}, errors.New("enter follow-up instructions")
	}
	path, err := s.Store.GetPath(ctx, pathID)
	if err != nil {
		return model.AgentRun{}, err
	}
	if provider == "" {
		if previous, previousErr := s.Store.LatestAgentRunForPath(ctx, pathID); previousErr == nil {
			provider = previous.Provider
		}
	}
	if !agentAvailable(ctx, provider) {
		return model.AgentRun{}, fmt.Errorf("%s is not available", provider)
	}
	selectedModel := ""
	if previous, previousErr := s.Store.LatestAgentRunForPath(ctx, pathID); previousErr == nil && previous.Provider == provider {
		selectedModel = previous.Model
	}
	run := model.AgentRun{ID: id.New("run"), TaskID: path.TaskID, PathID: path.ID, Provider: provider, Model: selectedModel, Prompt: prompt, State: model.RunQueued, CreatedAt: time.Now().UTC()}
	if err := s.Store.CreateAgentRun(ctx, run); err != nil {
		return model.AgentRun{}, err
	}
	s.Runs.Start(run, path, true)
	return run, nil
}

func (s *Service) AddVariations(ctx context.Context, pathID string, input AddVariationsInput) (model.TaskDetail, error) {
	if len(input.Variations) == 0 {
		return model.TaskDetail{}, errors.New("add at least one variation")
	}
	for _, variation := range input.Variations {
		if !agentAvailable(ctx, variation.Provider) {
			return model.TaskDetail{}, fmt.Errorf("%s is not available", variation.Provider)
		}
		if err := validateAgentModel(variation.Model); err != nil {
			return model.TaskDetail{}, err
		}
	}
	path, err := s.Store.GetPath(ctx, pathID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	before, err := s.Store.ListPaths(ctx, path.TaskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	existing := map[string]bool{}
	for _, item := range before {
		existing[item.ID] = true
	}
	names := make([]string, len(input.Variations))
	for index, variation := range input.Variations {
		names[index] = variationName(variation.Name, len(before)+index+1)
	}
	detail, err := s.ForkPath(ctx, path, names, "Started additional agent variations.")
	if err != nil {
		return model.TaskDetail{}, err
	}
	taskPrompt := strings.TrimSpace(input.Prompt)
	if taskPrompt == "" {
		taskPrompt = detail.Task.Description
	}
	created := make([]model.Path, 0, len(input.Variations))
	for _, item := range detail.Paths {
		if !existing[item.ID] {
			created = append(created, item)
		}
	}
	for index, item := range created {
		variation := input.Variations[index]
		prompt := taskPrompt
		if guidance := strings.TrimSpace(variation.Guidance); guidance != "" {
			prompt += "\n\nApproach-specific direction:\n" + guidance
		}
		run := model.AgentRun{ID: id.New("run"), TaskID: item.TaskID, PathID: item.ID, Provider: variation.Provider, Model: strings.TrimSpace(variation.Model), Prompt: prompt, State: model.RunQueued, CreatedAt: time.Now().UTC()}
		if err := s.Store.CreateAgentRun(ctx, run); err != nil {
			return model.TaskDetail{}, err
		}
		s.Runs.Start(run, item, false)
	}
	return s.TaskDetail(ctx, path.TaskID)
}

func (m *RunManager) Start(run model.AgentRun, path model.Path, resume bool) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.execute(run, path, resume)
	}()
}

func (m *RunManager) execute(run model.AgentRun, path model.Path, resume bool) {
	now := time.Now().UTC()
	run.State, run.StartedAt = model.RunRunning, &now
	_ = m.svc.Store.UpdateAgentRun(context.Background(), run)
	m.svc.Bus.Publish("agent.started", run.ID)

	logDir := filepath.Join(m.svc.StateDir, "runs")
	_ = os.MkdirAll(logDir, 0o700)
	logPath := filepath.Join(logDir, run.ID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		m.finishFailed(&run, err, -1)
		return
	}
	defer logFile.Close()

	lastPath := filepath.Join(logDir, run.ID+"-last.json")
	command, err := m.commandFor(run, path, resume, lastPath)
	if err != nil {
		m.finishFailed(&run, err, -1)
		return
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		m.finishFailed(&run, err, -1)
		return
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		m.finishFailed(&run, err, -1)
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.finishFailed(&run, errors.New("Ooh is stopping"), -1)
		return
	}
	if err := command.Start(); err != nil {
		m.mu.Unlock()
		m.finishFailed(&run, err, -1)
		return
	}
	m.active[run.ID] = command
	m.mu.Unlock()

	var outputMu sync.Mutex
	var finalText strings.Builder
	consume := func(reader io.Reader, prefix string) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 2<<20)
		for scanner.Scan() {
			line := scanner.Text()
			outputMu.Lock()
			_, _ = fmt.Fprintf(logFile, "%s%s\n", prefix, line)
			captureAgentEvent(line, &run, &finalText)
			outputMu.Unlock()
			m.svc.Bus.Publish("agent.output", run.ID)
		}
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); consume(stdout, "") }()
	go func() { defer readers.Done(); consume(stderr, "[stderr] ") }()
	waitErr := command.Wait()
	readers.Wait()
	m.removeActive(run.ID)
	exitCode := 0
	if waitErr != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if exitCode < 0 || exitCode == 137 || exitCode == 143 {
			finished := time.Now().UTC()
			run.State, run.ExitCode, run.Error, run.FinishedAt = model.RunStopped, exitCode, "Stopped by the user.", &finished
			_ = m.svc.Store.UpdateAgentRun(context.Background(), run)
			m.svc.Bus.Publish("agent.stopped", run.ID)
			return
		}
		m.finishFailed(&run, waitErr, exitCode)
		return
	}

	outcome := parseOutcomeFile(lastPath)
	if outcome.Summary == "" {
		outcome = parseOutcome(finalText.String())
	}
	if outcome.Summary == "" {
		outcome.Summary = "Agent completed the requested work."
	}
	if outcome.VerificationStatus != "passed" && outcome.VerificationStatus != "failed" && outcome.VerificationStatus != "not-run" {
		outcome.VerificationStatus = "not-run"
	}
	if strings.TrimSpace(outcome.VerificationNotes) == "" {
		outcome.VerificationNotes = "The agent did not report verification details."
	}
	run.Summary, run.VerificationStatus, run.VerificationNotes, run.RiskNotes = outcome.Summary, outcome.VerificationStatus, outcome.VerificationNotes, outcome.RiskNotes
	cp, checkpointErr := m.svc.CreateCheckpoint(context.Background(), path, CheckpointInput{Name: "Agent result", Summary: outcome.Summary, VerificationStatus: outcome.VerificationStatus, VerificationNotes: outcome.VerificationNotes, RiskNotes: outcome.RiskNotes, Ready: true})
	if checkpointErr != nil {
		m.finishFailed(&run, fmt.Errorf("agent finished, but Ooh could not save the result: %w", checkpointErr), 0)
		return
	}
	finished := time.Now().UTC()
	run.State, run.ExitCode, run.FinishedAt = model.RunSucceeded, 0, &finished
	_ = m.svc.Store.UpdateAgentRun(context.Background(), run)
	m.svc.Bus.Publish("agent.succeeded", cp.ID)
}

func (m *RunManager) commandFor(run model.AgentRun, path model.Path, resume bool, lastPath string) (*exec.Cmd, error) {
	wrapped := agentPrompt(run.Prompt)
	modelArgs := []string{}
	if run.Model != "" {
		modelArgs = []string{"--model", run.Model}
	}
	var command *exec.Cmd
	switch run.Provider {
	case "codex":
		schemaPath, err := m.ensureOutcomeSchema()
		if err != nil {
			return nil, err
		}
		if resume {
			previous, previousErr := m.previousRun(path, run.ID, "codex")
			if previousErr == nil && previous.ProviderSessionID != "" {
				args := append([]string{"exec", "resume", "--json", "--output-schema", schemaPath, "-o", lastPath}, modelArgs...)
				args = append(args, previous.ProviderSessionID, wrapped)
				command = exec.Command("codex", args...)
			}
		}
		if command == nil {
			args := append([]string{"exec", "--json", "-s", "workspace-write", "-C", path.WorktreePath, "--output-schema", schemaPath, "-o", lastPath}, modelArgs...)
			args = append(args, wrapped)
			command = exec.Command("codex", args...)
		}
	case "claude":
		schema := string(outcomeSchema())
		args := []string{"-p", "--output-format", "stream-json", "--include-partial-messages", "--permission-mode", "acceptEdits", "--json-schema", schema}
		args = append(args, modelArgs...)
		if resume {
			previous, previousErr := m.previousRun(path, run.ID, "claude")
			if previousErr == nil && previous.ProviderSessionID != "" {
				args = append(args, "--resume", previous.ProviderSessionID)
			}
		}
		args = append(args, wrapped)
		command = exec.Command("claude", args...)
	default:
		return nil, errors.New("unsupported agent provider")
	}
	command.Dir = path.WorktreePath
	command.Env = append(os.Environ(), "OOH_REPO_ROOT="+m.svc.Repo.Root, "OOH_PATH_ID="+path.ID, "GIT_TERMINAL_PROMPT=0")
	return command, nil
}

func (m *RunManager) previousRun(path model.Path, currentID, provider string) (model.AgentRun, error) {
	runs, err := m.svc.Store.ListAgentRuns(context.Background(), path.TaskID)
	if err != nil {
		return model.AgentRun{}, err
	}
	for index := len(runs) - 1; index >= 0; index-- {
		if runs[index].ID != currentID && runs[index].PathID == path.ID && runs[index].Provider == provider {
			return runs[index], nil
		}
	}
	return model.AgentRun{}, store.ErrNotFound
}

func (m *RunManager) ensureOutcomeSchema() (string, error) {
	path := filepath.Join(m.svc.StateDir, "agent-outcome.schema.json")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.WriteFile(path, outcomeSchema(), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func outcomeSchema() []byte {
	return []byte(`{"type":"object","additionalProperties":false,"properties":{"summary":{"type":"string"},"verificationStatus":{"type":"string","enum":["passed","failed","not-run"]},"verificationNotes":{"type":"string"},"riskNotes":{"type":"string"}},"required":["summary","verificationStatus","verificationNotes","riskNotes"]}`)
}

func agentPrompt(prompt string) string {
	return prompt + `

Work only inside the current isolated repository clone. Implement the requested change, inspect your work, and run appropriate checks. Do not merge, push, integrate, or call Ooh checkpoint commands. Your final response must be a JSON object containing summary, verificationStatus (passed, failed, or not-run), verificationNotes, and riskNotes.`
}

func (m *RunManager) finishFailed(run *model.AgentRun, err error, exitCode int) {
	finished := time.Now().UTC()
	run.State, run.ExitCode, run.Error, run.FinishedAt = model.RunFailed, exitCode, err.Error(), &finished
	_ = m.svc.Store.UpdateAgentRun(context.Background(), *run)
	m.svc.Bus.Publish("agent.failed", run.ID)
}

func (m *RunManager) removeActive(runID string) {
	m.mu.Lock()
	delete(m.active, runID)
	m.mu.Unlock()
}

func (m *RunManager) ReadLog(runID string, after int64) ([]byte, int64, error) {
	if after < 0 {
		after = 0
	}
	path := filepath.Join(m.svc.StateDir, "runs", runID+".log")
	file, err := os.Open(path)
	if err != nil {
		return nil, after, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, after, err
	}
	if after > info.Size() {
		after = 0
	}
	if _, err := file.Seek(after, io.SeekStart); err != nil {
		return nil, after, err
	}
	data, err := io.ReadAll(io.LimitReader(file, 512<<10))
	return data, after + int64(len(data)), err
}

func captureAgentEvent(line string, run *model.AgentRun, final *strings.Builder) {
	var value any
	if json.Unmarshal([]byte(line), &value) != nil {
		return
	}
	if run.ProviderSessionID == "" {
		run.ProviderSessionID = findString(value, "thread_id", "session_id", "sessionId")
	}
	text := findString(value, "result", "final_output", "output_text")
	if text != "" {
		final.Reset()
		final.WriteString(text)
	}
}

func findString(value any, keys ...string) string {
	wanted := map[string]bool{}
	for _, key := range keys {
		wanted[key] = true
	}
	var visit func(any) string
	visit = func(current any) string {
		switch typed := current.(type) {
		case map[string]any:
			for key, item := range typed {
				if wanted[key] {
					if text, ok := item.(string); ok {
						return text
					}
				}
			}
			for _, item := range typed {
				if found := visit(item); found != "" {
					return found
				}
			}
		case []any:
			for _, item := range typed {
				if found := visit(item); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return visit(value)
}

func parseOutcomeFile(path string) AgentOutcome {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentOutcome{}
	}
	return parseOutcome(string(data))
}

func parseOutcome(raw string) AgentOutcome {
	raw = strings.TrimSpace(raw)
	var outcome AgentOutcome
	if json.Unmarshal([]byte(raw), &outcome) == nil {
		return outcome
	}
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		_ = json.Unmarshal([]byte(raw[start:end+1]), &outcome)
	}
	return outcome
}

func firstAvailableAgent(ctx context.Context) string {
	for _, info := range AvailableAgents(ctx) {
		if info.Available {
			return info.ID
		}
	}
	return ""
}

func agentAvailable(ctx context.Context, provider string) bool {
	for _, info := range AvailableAgents(ctx) {
		if info.ID == provider {
			return info.Available
		}
	}
	return false
}

func validateAgentModel(value string) error {
	value = strings.TrimSpace(value)
	if len(value) > 120 || strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("the selected agent model is invalid")
	}
	return nil
}

func variationName(raw string, index int) string {
	if name := strings.TrimSpace(raw); name != "" {
		return name
	}
	return fmt.Sprintf("Approach %d", index)
}

var titleSpaces = regexp.MustCompile(`\s+`)

func titleFromInstructions(instructions string) string {
	line := strings.TrimSpace(strings.SplitN(instructions, "\n", 2)[0])
	line = titleSpaces.ReplaceAllString(line, " ")
	line = strings.TrimLeftFunc(line, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	if line == "" {
		return "New task"
	}
	const max = 64
	if utf8.RuneCountInString(line) > max {
		runes := []rune(line)
		line = strings.TrimSpace(string(runes[:max-1])) + "…"
	}
	return line
}

func IsStoreNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
