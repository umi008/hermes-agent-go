// Package files ports the upstream file toolset: read_file, write_file,
// patch, and search_files (tools/file_tools.py).
package files

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Toolset is the upstream toolset name.
const Toolset = "file"

// Files is the shared state of one session's file tools: where relative
// paths resolve, and which files the agent has read in full.
type Files struct {
	// Cwd returns the directory relative paths resolve against.
	Cwd func() string
	// Home expands a leading "~".
	Home string

	mu    sync.Mutex
	reads map[string]stamp
}

type stamp struct {
	mod  time.Time
	size int64
}

// New builds the file tools state.
func New(cwd func() string, home string) *Files {
	return &Files{Cwd: cwd, Home: home, reads: map[string]stamp{}}
}

// Tools returns the four file tools.
func (f *Files) Tools() []tools.Tool {
	return []tools.Tool{f.readTool(), f.writeTool(), f.patchTool(), f.searchTool()}
}

func (f *Files) resolve(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		p = filepath.Join(f.Home, p[1:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.Cwd(), p)
	}
	return filepath.Clean(p)
}

// remember records that path's current content is known to the agent.
func (f *Files) remember(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[path] = stamp{mod: info.ModTime(), size: info.Size()}
}

// current reports whether the agent's last full read or write of path still
// matches the file on disk.
func (f *Files) current(path string, info os.FileInfo) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.reads[path]
	return ok && s.mod.Equal(info.ModTime()) && s.size == info.Size()
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }
