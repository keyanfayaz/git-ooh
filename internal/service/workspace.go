package service

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/keyanfayaz/git-ooh/internal/model"
)

const maxEditableFile = 2 << 20

var ErrFileConflict = errors.New("the file changed outside the editor")

type TreeEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Size     int64  `json:"size,omitempty"`
	Modified string `json:"modified,omitempty"`
}

type WorkspaceFile struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Revision  string `json:"revision"`
	Language  string `json:"language"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
}

type SearchMatch struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Preview string `json:"preview"`
}

func (s *Service) workspacePath(ctx context.Context, pathID, relative string, allowMissing bool) (model.Path, string, error) {
	p, err := s.Store.GetPath(ctx, pathID)
	if err != nil {
		return model.Path{}, "", err
	}
	if p.WorktreePath == "" || p.State == model.PathArchived || p.State == model.PathDeleted {
		return model.Path{}, "", errors.New("this workspace is not available")
	}
	relative = filepath.Clean(filepath.FromSlash(strings.TrimSpace(relative)))
	if relative == "." {
		relative = ""
	}
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return model.Path{}, "", errors.New("file path must stay inside the workspace")
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == ".git" {
			return model.Path{}, "", errors.New("Git internals are not available in the editor")
		}
	}
	target := filepath.Join(p.WorktreePath, relative)
	check := target
	if allowMissing {
		check = filepath.Dir(target)
	}
	realRoot, err := filepath.EvalSymlinks(p.WorktreePath)
	if err != nil {
		return model.Path{}, "", err
	}
	realCheck, err := filepath.EvalSymlinks(check)
	if err != nil {
		return model.Path{}, "", err
	}
	rel, err := filepath.Rel(realRoot, realCheck)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return model.Path{}, "", errors.New("symbolic links outside the workspace are not available")
	}
	return p, target, nil
}

func (s *Service) ListWorkspaceTree(ctx context.Context, pathID, relative string) ([]TreeEntry, error) {
	_, target, err := s.workspacePath(ctx, pathID, relative, false)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}
	out := make([]TreeEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		}
		child := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		out = append(out, TreeEntry{Name: entry.Name(), Path: strings.TrimPrefix(child, "./"), Kind: kind, Size: info.Size(), Modified: info.ModTime().UTC().Format("2006-01-02T15:04:05.999999999Z")})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "directory"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (s *Service) ReadWorkspaceFile(ctx context.Context, pathID, relative string) (WorkspaceFile, error) {
	_, target, err := s.workspacePath(ctx, pathID, relative, false)
	if err != nil {
		return WorkspaceFile{}, err
	}
	file, err := os.Open(target)
	if err != nil {
		return WorkspaceFile{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return WorkspaceFile{}, err
	}
	if !info.Mode().IsRegular() {
		return WorkspaceFile{}, errors.New("only regular files can be opened")
	}
	if info.Size() > maxEditableFile {
		return WorkspaceFile{}, fmt.Errorf("file is larger than %d MB", maxEditableFile>>20)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return WorkspaceFile{}, err
	}
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return WorkspaceFile{}, errors.New("binary files cannot be edited as text")
	}
	mediaType := mime.TypeByExtension(filepath.Ext(target))
	if mediaType == "" {
		mediaType = "text/plain"
	}
	return WorkspaceFile{Path: filepath.ToSlash(relative), Content: string(data), Revision: fileRevision(data), Language: languageForPath(relative), MediaType: mediaType, Size: info.Size()}, nil
}

func (s *Service) OpenWorkspaceRaw(ctx context.Context, pathID, relative string) (*os.File, os.FileInfo, error) {
	_, target, err := s.workspacePath(ctx, pathID, relative, false)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err == nil {
			err = errors.New("only regular files can be opened")
		}
		return nil, nil, err
	}
	return file, info, nil
}

func (s *Service) WriteWorkspaceFile(ctx context.Context, pathID, relative, content, expectedRevision string) (WorkspaceFile, error) {
	_, target, err := s.workspacePath(ctx, pathID, relative, false)
	if err != nil {
		return WorkspaceFile{}, err
	}
	info, err := os.Lstat(target)
	if err != nil {
		return WorkspaceFile{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return WorkspaceFile{}, errors.New("only regular, non-symbolic-link files can be edited")
	}
	current, err := os.ReadFile(target)
	if err != nil {
		return WorkspaceFile{}, err
	}
	if expectedRevision == "" || fileRevision(current) != expectedRevision {
		return WorkspaceFile{}, ErrFileConflict
	}
	if len(content) > maxEditableFile {
		return WorkspaceFile{}, fmt.Errorf("file is larger than %d MB", maxEditableFile>>20)
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".ooh-save-*")
	if err != nil {
		return WorkspaceFile{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(info.Mode().Perm()); err == nil {
		_, err = io.WriteString(temporary, content)
	}
	closeErr := temporary.Close()
	if err != nil {
		return WorkspaceFile{}, err
	}
	if closeErr != nil {
		return WorkspaceFile{}, closeErr
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return WorkspaceFile{}, err
	}
	s.Bus.Publish("workspace.changed", pathID)
	return s.ReadWorkspaceFile(ctx, pathID, relative)
}

func (s *Service) CreateWorkspaceEntry(ctx context.Context, pathID, relative, kind string) error {
	_, target, err := s.workspacePath(ctx, pathID, relative, true)
	if err != nil {
		return err
	}
	if kind == "directory" {
		err = os.Mkdir(target, 0o700)
	} else {
		var file *os.File
		file, err = os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			err = file.Close()
		}
	}
	if err == nil {
		s.Bus.Publish("workspace.changed", pathID)
	}
	return err
}

func (s *Service) RenameWorkspaceEntry(ctx context.Context, pathID, relative, destination string) error {
	_, source, err := s.workspacePath(ctx, pathID, relative, false)
	if err != nil {
		return err
	}
	_, target, err := s.workspacePath(ctx, pathID, destination, true)
	if err != nil {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		return err
	}
	s.Bus.Publish("workspace.changed", pathID)
	return nil
}

func (s *Service) DeleteWorkspaceEntry(ctx context.Context, pathID, relative string) error {
	if strings.TrimSpace(relative) == "" || filepath.Clean(relative) == "." {
		return errors.New("the workspace root cannot be deleted")
	}
	_, target, err := s.workspacePath(ctx, pathID, relative, false)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	s.Bus.Publish("workspace.changed", pathID)
	return nil
}

func (s *Service) SearchWorkspace(ctx context.Context, pathID, query string) ([]SearchMatch, error) {
	query = strings.TrimSpace(query)
	if len(query) < 2 {
		return []SearchMatch{}, nil
	}
	_, root, err := s.workspacePath(ctx, pathID, "", false)
	if err != nil {
		return nil, err
	}
	lowerQuery := strings.ToLower(query)
	results := make([]SearchMatch, 0)
	skipDirs := map[string]bool{".git": true, "node_modules": true, ".next": true, "dist": true, "build": true, "target": true, "vendor": true}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || len(results) >= 100 {
			return nil
		}
		if entry.IsDir() {
			if path != root && skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		scanner := bufio.NewScanner(file)
		line := 0
		for scanner.Scan() && len(results) < 100 {
			line++
			text := scanner.Text()
			if strings.Contains(strings.ToLower(text), lowerQuery) {
				rel, _ := filepath.Rel(root, path)
				results = append(results, SearchMatch{Path: filepath.ToSlash(rel), Line: line, Preview: strings.TrimSpace(text)})
			}
		}
		_ = file.Close()
		return nil
	})
	return results, err
}

func fileRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func languageForPath(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	known := map[string]string{".js": "javascript", ".jsx": "javascript", ".ts": "typescript", ".tsx": "typescript", ".json": "json", ".css": "css", ".html": "html", ".md": "markdown", ".go": "go", ".py": "python", ".rb": "ruby", ".rs": "rust", ".sh": "shell", ".yaml": "yaml", ".yml": "yaml", ".toml": "toml", ".sql": "sql", ".xml": "xml"}
	if language := known[ext]; language != "" {
		return language
	}
	return "text"
}
