package gittool

import (
	"context"
	"errors"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const fileHistoryFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/filehistory.FileHistory"

var fileHistoryTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c018",
	Slug:          "gitfilehistory",
	Version:       spec.VersionOne,
	DisplayName:   "Git file history",
	Description:   "List bounded commit history touching one repository-relative path.",
	Tags:          []string{toolTagGit, toolTagRead},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	},
	"path": {
		"type": "string",
		"description": "Repository-relative file path."
	},
	"revision": {
		"type": "string",
		"description": "Revision to start from.",
		"default": "HEAD"
	},
	"maxCommits": {
		"type": "integer",
		"description": "Maximum matching commits to return.",
		"default": 50,
		"minimum": 1,
		"maximum": 500
	},
	"maxWalk": {
		"type": "integer",
		"description": "Maximum commits to inspect while looking for matching commits.",
		"default": 2000,
		"minimum": 1,
		"maximum": 20000
	},
	"firstParent": {
		"type": "boolean",
		"description": "If true, follow only the first-parent chain. Defaults to true.",
		"default": true
	}
},
"required": ["repoPath", "path"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: fileHistoryFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type FileHistoryArgs struct {
	RepoPath    string `json:"repoPath"`
	Path        string `json:"path"`
	Revision    string `json:"revision,omitempty"`
	MaxCommits  int    `json:"maxCommits,omitempty"`
	MaxWalk     int    `json:"maxWalk,omitempty"`
	FirstParent *bool  `json:"firstParent,omitempty"`
}

type FileHistoryCommit struct {
	Commit CommitInfo `json:"commit"`
	Path   string     `json:"path"`
	Status string     `json:"status"`
}

type FileHistoryOut struct {
	RepoPath        string              `json:"repoPath"`
	Path            string              `json:"path"`
	Revision        string              `json:"revision"`
	MaxCommits      int                 `json:"maxCommits"`
	MaxWalk         int                 `json:"maxWalk"`
	FirstParent     bool                `json:"firstParent"`
	Commits         []FileHistoryCommit `json:"commits"`
	Count           int                 `json:"count"`
	Walked          int                 `json:"walked"`
	Truncated       bool                `json:"truncated"`
	RenameFollowing bool                `json:"renameFollowing"`
	Note            string              `json:"note,omitempty"`
}

func fileHistory(ctx context.Context, snap gitToolSnapshot, args FileHistoryArgs) (*FileHistoryOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p, err := normalizeRepoRelativePath(args.Path)
	if err != nil {
		return nil, err
	}
	revision := strings.TrimSpace(args.Revision)
	if revision == "" {
		revision = revisionHead
	}
	maxCommits := normalizePositiveInt(args.MaxCommits, defaultHistoryMaxCount, 1, hardHistoryMaxCount)
	maxWalk := normalizePositiveInt(args.MaxWalk, defaultHistoryMaxWalk, 1, hardHistoryMaxWalk)
	firstParent := true
	if args.FirstParent != nil {
		firstParent = *args.FirstParent
	}

	repo, abs, err := openRepository(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}
	start, err := resolveCommit(repo, revision)
	if err != nil {
		return nil, err
	}

	startTree, err := start.Tree()
	if err != nil {
		return nil, err
	}
	if !treePathBlobIdentity(startTree, p).Exists {
		return nil, errors.New("path does not exist at revision")
	}

	out := &FileHistoryOut{
		RepoPath:        abs,
		Path:            p,
		Revision:        revision,
		MaxCommits:      maxCommits,
		MaxWalk:         maxWalk,
		FirstParent:     firstParent,
		RenameFollowing: false,
		Note:            "rename following is not implemented in this version",
	}

	if firstParent {
		if err := walkFileHistoryFirstParent(ctx, start, p, out); err != nil {
			return nil, err
		}
	} else {
		if err := walkFileHistoryAllParents(ctx, start, p, out); err != nil {
			return nil, err
		}
	}

	out.Count = len(out.Commits)
	out.Truncated = out.Walked >= maxWalk && len(out.Commits) < maxCommits
	return out, nil
}

func walkFileHistoryFirstParent(
	ctx context.Context,
	start *object.Commit,
	p string,
	out *FileHistoryOut,
) error {
	cur := start

	for cur != nil && out.Walked < out.MaxWalk && len(out.Commits) < out.MaxCommits {
		if err := ctx.Err(); err != nil {
			return err
		}
		out.Walked++
		touched, status, err := commitTouchesPathFirstParent(cur, p)
		if err != nil {
			return err
		}
		if touched {
			out.Commits = append(out.Commits, FileHistoryCommit{
				Commit: commitInfo(cur),
				Path:   p,
				Status: status,
			})
		}
		if cur.NumParents() == 0 {
			break
		}
		parent, err := cur.Parent(0)
		if err != nil {
			break
		}
		cur = parent

	}
	//nolint:nilerr // No parent found.
	return nil
}

func walkFileHistoryAllParents(
	ctx context.Context,
	start *object.Commit,
	p string,
	out *FileHistoryOut,
) error {
	seen := make(map[string]bool)
	queue := []*object.Commit{start}
	for len(queue) > 0 && out.Walked < out.MaxWalk && len(out.Commits) < out.MaxCommits {
		if err := ctx.Err(); err != nil {
			return err
		}
		cur := queue[0]
		queue = queue[1:]
		if cur == nil {
			continue
		}
		hash := cur.Hash.String()
		if seen[hash] {
			continue
		}
		seen[hash] = true
		out.Walked++
		touched, status, err := commitTouchesPathAnyParent(cur, p)
		if err != nil {
			return err
		}
		if touched {
			out.Commits = append(out.Commits, FileHistoryCommit{
				Commit: commitInfo(cur),
				Path:   p,
				Status: status,
			})
		}
		for i := 0; i < cur.NumParents(); i++ {
			parent, err := cur.Parent(i)
			if err != nil {
				continue
			}
			queue = append(queue, parent)
		}
	}
	return nil
}

func commitTouchesPathAnyParent(c *object.Commit, p string) (touched bool, status string, err error) {
	if c.NumParents() == 0 {
		return commitTouchesPathFirstParent(c, p)
	}
	for i := 0; i < c.NumParents(); i++ {
		parent, err := c.Parent(i)
		if err != nil {
			continue
		}
		tree, err := c.Tree()
		if err != nil {
			return false, "", err
		}
		parentTree, err := parent.Tree()
		if err != nil {
			return false, "", err
		}
		cur := treePathBlobIdentity(tree, p)
		prev := treePathBlobIdentity(parentTree, p)
		touched, status, err := identitiesDiffer(cur, prev)
		if err != nil {
			return false, "", err
		}
		if touched {
			return true, status, nil
		}
	}
	return false, "", nil
}

func commitTouchesPathFirstParent(c *object.Commit, p string) (touched bool, status string, err error) {
	tree, err := c.Tree()
	if err != nil {
		return false, "", err
	}
	cur := treePathBlobIdentity(tree, p)
	if c.NumParents() == 0 {
		if cur.Exists {
			return true, "added", nil
		}
		return false, "", nil
	}
	parent, err := c.Parent(0)
	if err != nil {
		return false, "", err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return false, "", err
	}
	prev := treePathBlobIdentity(parentTree, p)
	return identitiesDiffer(cur, prev)
}

func identitiesDiffer(cur, prev treePathIdentity) (touched bool, status string, err error) {
	switch {
	case !prev.Exists && cur.Exists:
		return true, "added", nil
	case prev.Exists && !cur.Exists:
		return true, "deleted", nil
	case prev.Exists && cur.Exists && (prev.Hash != cur.Hash || prev.Mode != cur.Mode):
		return true, "modified", nil
	case prev.Exists == cur.Exists:
		return false, "", nil
	default:
		return false, "", errors.New("unreachable path identity state")
	}
}
