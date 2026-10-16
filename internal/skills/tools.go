package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	agentskillsSpec "github.com/flexigpt/agentskills-go/runtime/spec"
)

const skillName = "skill_name"

// sessionContextKey is the context key for the skills session ID.
type sessionContextKey struct{}

// WithSessionID returns a context with the skills session ID attached.
func WithSessionID(ctx context.Context, sid agentskillsSpec.SessionID) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, sid)
}

// GetSessionID retrieves the skills session ID from context.
func GetSessionID(ctx context.Context) (agentskillsSpec.SessionID, bool) {
	sid, ok := ctx.Value(sessionContextKey{}).(agentskillsSpec.SessionID)

	return sid, ok
}

// Compile-time interface assertions.
var (
	_ tool.InvokableTool = (*LoadTool)(nil)
	_ tool.InvokableTool = (*UnloadTool)(nil)
	_ tool.InvokableTool = (*ReadSkillAssetTool)(nil)
	_ tool.InvokableTool = (*ListSkillAssetsTool)(nil)
)

// skillsLoadArgs is the input structure for the skills-load tool.
type skillsLoadArgs struct {
	SkillName string `json:"skill_name"`
}

// LoadTool implements tool.InvokableTool for loading a skill.
type LoadTool struct {
	manager *Manager
}

// NewSkillsLoadTool creates a new LoadTool.
func NewSkillsLoadTool(manager *Manager) *LoadTool {
	return &LoadTool{manager: manager}
}

// Info returns the tool metadata for the LLM.
func (t *LoadTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "skills-load",
		Desc: "Load full content of a skill by name. Use when you need detailed information about a topic " +
			"(e.g., home layout, formatting rules). Available skills are listed in the system prompt.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			skillName: {
				Type:     schema.String,
				Desc:     "Name of the skill to load (e.g., 'home-topology', 'telegram-formatter')",
				Required: true,
			},
		}),
	}, nil
}

// InvokableRun executes the skill load operation.
func (t *LoadTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args skillsLoadArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("failed to parse skills-load arguments: %w", err)
	}

	if args.SkillName == "" {
		return "", fmt.Errorf("failed to load skill: skill_name is required")
	}

	sessionID, ok := GetSessionID(ctx)
	if !ok {
		return "", fmt.Errorf("failed to load skill: no session ID in context")
	}

	content, err := t.manager.LoadSkill(ctx, sessionID, args.SkillName)
	if err != nil {
		return "", err
	}

	return content, nil
}

// skillsUnloadArgs is the input structure for the skills-unload tool.
type skillsUnloadArgs struct {
	SkillName string `json:"skill_name"`
}

// UnloadTool implements tool.InvokableTool for unloading a skill.
type UnloadTool struct {
	manager *Manager
}

// NewSkillsUnloadTool creates a new UnloadTool.
func NewSkillsUnloadTool(manager *Manager) *UnloadTool {
	return &UnloadTool{manager: manager}
}

// Info returns the tool metadata for the LLM.
func (t *UnloadTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "skills-unload",
		Desc: "Unload a previously loaded skill from the current session to free context space. " +
			"Use when you no longer need the detailed information from a skill.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			skillName: {
				Type:     schema.String,
				Desc:     "Name of the skill to unload (e.g., 'home-topology')",
				Required: true,
			},
		}),
	}, nil
}

// InvokableRun executes the skill unload operation.
func (t *UnloadTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args skillsUnloadArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("failed to parse skills-unload arguments: %w", err)
	}

	if args.SkillName == "" {
		return "", fmt.Errorf("failed to unload skill: skill_name is required")
	}

	sessionID, ok := GetSessionID(ctx)
	if !ok {
		return "", fmt.Errorf("failed to unload skill: no session ID in context")
	}

	if err := t.manager.UnloadSkill(ctx, sessionID, args.SkillName); err != nil {
		return "", err
	}

	return fmt.Sprintf("Skill %q unloaded successfully.", args.SkillName), nil
}

type skillsAssetReadArgs struct {
	SkillName string `json:"skill_name"`
	AssetPath string `json:"asset_path"`
}

// ReadSkillAssetTool implements tool.InvokableTool for reading a specific file within a skill's directory.
type ReadSkillAssetTool struct {
	manager *Manager
	cache   interface {
		Load(context.Context, string) (string, error)
	}
}

// NewReadSkillAssetTool creates a new ReadSkillAssetTool.
func NewReadSkillAssetTool(manager *Manager, cache interface {
	Load(context.Context, string) (string, error)
},
) *ReadSkillAssetTool {
	return &ReadSkillAssetTool{manager: manager, cache: cache}
}

// Info returns the tool metadata for the LLM.
func (t *ReadSkillAssetTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "skills-read-asset",
		Desc: "Read a specific asset file from a skill's directory. Use this when a skill references an asset (e.g., 'assets/devices.md') and you need to see its content.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			skillName: {
				Type:     schema.String,
				Desc:     "Name of the skill (e.g., 'home-topology')",
				Required: true,
			},
			"asset_path": {
				Type:     schema.String,
				Desc:     "Path to the asset relative to the skill root (e.g., 'assets/devices.md')",
				Required: true,
			},
		}),
	}, nil
}

// InvokableRun executes the asset read operation.
func (t *ReadSkillAssetTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args skillsAssetReadArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("failed to parse skills-read-asset arguments: %w", err)
	}

	if args.SkillName == "" || args.AssetPath == "" {
		return "", fmt.Errorf("failed to read skill asset: skill_name and asset_path are required")
	}

	skillDir, err := t.manager.GetSkillLocation(args.SkillName)
	if err != nil {
		return "", err
	}

	// Resolve and validate path to prevent traversal.
	absAssetPath := filepath.Join(skillDir, args.AssetPath)
	resolvedPath, err := filepath.Abs(absAssetPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve asset path: %w", err)
	}

	// Ensure the resolved path is within the skill directory.
	// criticalities-remediation (M11): the containment check must include a
	// path-separator boundary — a bare strings.HasPrefix would let the skill
	// at <runtime home>/skills/foo expose sibling directories sharing its name prefix
	// (<runtime home>/skills/foo-bar). filepath.Rel is the robust containment test:
	// a relative result that does not start with ".." (and is not absolute)
	// stays inside the skill's own directory.
	rel, relErr := filepath.Rel(skillDir, resolvedPath)
	if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("security error: asset path %q is outside the skill directory %q", args.AssetPath, skillDir)
	}

	content, err := t.cache.Load(ctx, resolvedPath)
	if err != nil {
		return "", fmt.Errorf("failed to load skill asset %q: %w", args.AssetPath, err)
	}

	return content, nil
}

type skillsAssetListArgs struct {
	SkillName string `json:"skill_name"`
}

// ListSkillAssetsTool implements tool.InvokableTool for listing files within a skill's directory.
type ListSkillAssetsTool struct {
	manager *Manager
}

// NewListSkillAssetsTool creates a new ListSkillAssetsTool.
func NewListSkillAssetsTool(manager *Manager) *ListSkillAssetsTool {
	return &ListSkillAssetsTool{manager: manager}
}

// Info returns the tool metadata for the LLM.
func (t *ListSkillAssetsTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "skills-list-assets",
		Desc: "List all files and directories within a skill's directory. Use this to discover available assets for a skill.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			skillName: {
				Type:     schema.String,
				Desc:     "Name of the skill (e.g., 'home-topology')",
				Required: true,
			},
		}),
	}, nil
}

// InvokableRun executes the asset list operation.
func (t *ListSkillAssetsTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args skillsAssetListArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("failed to parse skills-list-assets arguments: %w", err)
	}

	if args.SkillName == "" {
		return "", fmt.Errorf("failed to list skill assets: skill_name is required")
	}

	skillDir, err := t.manager.GetSkillLocation(args.SkillName)
	if err != nil {
		return "", err
	}

	assets, err := collectSkillAssets(ctx, skillDir)
	if err != nil {
		return "", err
	}

	if len(assets) == 0 {
		return "No additional assets found for this skill.", nil
	}

	return fmt.Sprintf("Assets available for skill %q:\n%s", args.SkillName, strings.Join(assets, "\n")), nil
}

// collectSkillAssets walks skillDir and returns the paths of every entry it
// contains, relative to skillDir. The skill root itself and its SKILL.md are
// excluded. The walk aborts as soon as ctx is cancelled.
func collectSkillAssets(ctx context.Context, skillDir string) ([]string, error) {
	var assets []string

	err := filepath.WalkDir(skillDir, func(path string, d os.DirEntry, walkErr error) error {
		switch {
		case walkErr != nil:
			return walkErr
		case ctx.Err() != nil:
			return ctx.Err()
		case isSkillAssetExcluded(skillDir, path, d):
			return nil
		}

		rel, relErr := filepath.Rel(skillDir, path)
		if relErr != nil {
			return relErr
		}
		assets = append(assets, rel)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk skill directory: %w", err)
	}

	return assets, nil
}

// isSkillAssetExcluded reports whether path must be left out of the asset
// listing: the skill root directory and the skill's own SKILL.md file.
func isSkillAssetExcluded(skillDir, path string, d os.DirEntry) bool {
	if path == skillDir {
		return true
	}

	return !d.IsDir() && filepath.Base(path) == "SKILL.md"
}
