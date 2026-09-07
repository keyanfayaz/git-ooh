package agentskill

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	skillassets "github.com/keyanfayaz/git-ooh/skills"
)

type InstallResult struct {
	Destination string   `json:"destination"`
	Files       []string `json:"files"`
	Changed     bool     `json:"changed"`
}

func Preview(projectRoot string, userScope bool) (InstallResult, error) {
	destination, err := destination(projectRoot, userScope)
	if err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Destination: destination, Files: []string{"SKILL.md", "agents/openai.yaml"}}, nil
}

func Install(projectRoot string, userScope bool, force bool) (InstallResult, error) {
	result, err := Preview(projectRoot, userScope)
	if err != nil {
		return result, err
	}
	if _, err := os.Stat(result.Destination); err == nil && !force {
		return result, fmt.Errorf("skill already exists at %s; pass --force to replace it", result.Destination)
	}
	if err := os.MkdirAll(filepath.Join(result.Destination, "agents"), 0o755); err != nil {
		return result, err
	}
	for _, name := range result.Files {
		data, err := fs.ReadFile(skillassets.Embedded, filepath.ToSlash(filepath.Join("ooh", name)))
		if err != nil {
			return result, err
		}
		target := filepath.Join(result.Destination, filepath.FromSlash(name))
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return result, err
		}
	}
	result.Changed = true
	return result, nil
}

func destination(projectRoot string, userScope bool) (string, error) {
	if userScope {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".agents", "skills", "ooh"), nil
	}
	if strings.TrimSpace(projectRoot) == "" {
		return "", fmt.Errorf("project root is required for project-scope installation")
	}
	return filepath.Join(projectRoot, ".agents", "skills", "ooh"), nil
}
