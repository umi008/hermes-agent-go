package files

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

func (f *Files) writeTool() tools.Tool {
	return tools.Tool{
		Name:    "write_file",
		Toolset: Toolset,
		Description: "Write content to a file, completely replacing existing content. Use this instead of echo/cat heredoc in terminal. " +
			"Creates parent directories automatically. OVERWRITES the entire file — use 'patch' for targeted edits. " +
			"For an EXISTING file, call read_file first: write_file refuses (file untouched) when this task has no current full read/write of the file " +
			"or the file changed on disk since; on refusal, read_file, merge, then retry.",
		Parameters: schema(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"Path to the file to write (will be created if it doesn't exist, overwritten if it does)"},` +
			`"content":{"type":"string","description":"Complete content to write to the file"}` +
			`},"required":["path","content"]}`),
		Handler: f.write,
	}
}

type writeResult struct {
	BytesWritten int  `json:"bytes_written"`
	DirsCreated  bool `json:"dirs_created"`
	Verified     bool `json:"verified"`
}

func (f *Files) write(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path    *string `json:"path"`
		Content *string `json:"content"`
	}
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	if a.Path == nil || *a.Path == "" {
		return "", tools.Error("write_file requires 'path'")
	}
	if a.Content == nil {
		return "", tools.Error("write_file requires 'content' (pass an empty string to truncate the file)")
	}
	path := f.resolve(*a.Path)
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		return "", tools.Error(*a.Path + " is a directory")
	case err == nil && !f.current(path, info):
		return "", tools.Error("Refusing to overwrite " + *a.Path + ": it exists and this task has no current full read of it, " +
			"or it changed on disk since. Call read_file, merge your changes, then retry.")
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", tools.Error(err.Error())
	}
	created, err := ensureDir(filepath.Dir(path))
	if err != nil {
		return "", tools.Error(err.Error())
	}
	if err := writeKeepingMode(path, []byte(*a.Content)); err != nil {
		return "", tools.Error(err.Error())
	}
	f.remember(path)
	data, err := os.ReadFile(path)
	return tools.Result(writeResult{BytesWritten: len(*a.Content), DirsCreated: created, Verified: err == nil && string(data) == *a.Content})
}

func ensureDir(dir string) (bool, error) {
	if _, err := os.Stat(dir); err == nil {
		return false, nil
	}
	return true, os.MkdirAll(dir, 0o755)
}

// writeKeepingMode replaces path's content, preserving an existing mode.
func writeKeepingMode(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, data, mode)
}
