package gittool

import (
	"context"
	"sort"

	"github.com/flexigpt/llmtools-go/spec"
)

const statusFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/status.Status"

var statusTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c002",
	Slug:          "gitstatus",
	Version:       spec.VersionOne,
	DisplayName:   "Git status",
	Description:   "Return structured local Git repository status, including branch, HEAD, remotes, author config, and worktree entries.",
	Tags:          []string{toolTagGit, toolTagRead},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: statusFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type StatusArgs struct {
	RepoPath string `json:"repoPath"`
}

type RemoteInfo struct {
	Name string   `json:"name"`
	URLs []string `json:"urls"`
}

type StatusEntry struct {
	Path     string `json:"path"`
	Staging  string `json:"staging"`
	Worktree string `json:"worktree"`
}

type StatusOut struct {
	RepoPath           string         `json:"repoPath"`
	HeadHash           string         `json:"headHash,omitempty"`
	Branch             string         `json:"branch,omitempty"`
	DetachedHead       bool           `json:"detachedHead"`
	UnbornHead         bool           `json:"unbornHead"`
	IsClean            bool           `json:"isClean"`
	DefaultAuthorName  string         `json:"defaultAuthorName,omitempty"`
	DefaultAuthorEmail string         `json:"defaultAuthorEmail,omitempty"`
	Remotes            []RemoteInfo   `json:"remotes,omitempty"`
	Entries            []StatusEntry  `json:"entries,omitempty"`
	Summary            map[string]int `json:"summary,omitempty"`
	IndexState         IndexState     `json:"indexState"`
}

func status(ctx context.Context, snap gitToolSnapshot, args StatusArgs) (*StatusOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	repo, wt, abs, err := openWorktree(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	st, err := wt.Status()
	if err != nil {
		return nil, err
	}

	branch, hash, detached, unborn := headInfoDetailed(repo)
	out := &StatusOut{
		RepoPath:     abs,
		HeadHash:     hash,
		Branch:       branch,
		DetachedHead: detached,
		UnbornHead:   unborn,
		IsClean:      st.IsClean(),
		Summary:      make(map[string]int),
	}
	out.IndexState, _ = detectIndexState(repo, wt)

	paths := make([]string, 0, len(st))
	for p := range st {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		fs := st[p]
		entry := StatusEntry{
			Path:     p,
			Staging:  string(fs.Staging),
			Worktree: string(fs.Worktree),
		}
		out.Entries = append(out.Entries, entry)
		out.Summary["staging:"+entry.Staging]++
		out.Summary["worktree:"+entry.Worktree]++
	}

	if len(out.Summary) == 0 {
		out.Summary = nil
	}

	if cfg, err := repo.Config(); err == nil && cfg != nil {
		out.DefaultAuthorName = cfg.User.Name
		out.DefaultAuthorEmail = cfg.User.Email
		remoteNames := make([]string, 0, len(cfg.Remotes))
		for name := range cfg.Remotes {
			remoteNames = append(remoteNames, name)
		}
		sort.Strings(remoteNames)
		for _, name := range remoteNames {
			remote := cfg.Remotes[name]
			if remote == nil {
				continue
			}
			out.Remotes = append(out.Remotes, RemoteInfo{Name: name, URLs: append([]string(nil), remote.URLs...)})
		}
	}

	return out, nil
}
