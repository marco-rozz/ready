package texttool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const (
	applyUnifiedDiffFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/texttool/applyunifieddiff.ApplyUnifiedDiff"

	statusCanBeApplied      = "Unified diff can be applied."
	errNoFilesWereProcessed = "No files were processed."
	pathDevNull             = "/dev/null"

	defaultUnifiedDiffNewFilePerm os.FileMode = 0o644
)

type ApplyUnifiedDiffStatus string

const (
	ApplyUnifiedDiffStatusApplicable     ApplyUnifiedDiffStatus = "applicable"
	ApplyUnifiedDiffStatusApplied        ApplyUnifiedDiffStatus = "applied"
	ApplyUnifiedDiffStatusAlreadyApplied ApplyUnifiedDiffStatus = "already_applied"
	ApplyUnifiedDiffStatusNeedsInfo      ApplyUnifiedDiffStatus = "needs_info"
	ApplyUnifiedDiffStatusConflict       ApplyUnifiedDiffStatus = "conflict"
	ApplyUnifiedDiffStatusError          ApplyUnifiedDiffStatus = "error"
)

type ApplyUnifiedDiffDiagnosticLevel string

const (
	ApplyUnifiedDiffDiagnosticLevelInfo    ApplyUnifiedDiffDiagnosticLevel = "info"
	ApplyUnifiedDiffDiagnosticLevelWarning ApplyUnifiedDiffDiagnosticLevel = "warning"
	ApplyUnifiedDiffDiagnosticLevelError   ApplyUnifiedDiffDiagnosticLevel = "error"
)

type ApplyUnifiedDiffDiagnostic struct {
	Level   ApplyUnifiedDiffDiagnosticLevel `json:"level"`
	Code    string                          `json:"code,omitempty"`
	Message string                          `json:"message"`
}

var applyUnifiedDiffTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019d35a3-9a6c-77d7-86a2-7203b07457fc",
	Slug:          "applyunifieddiff",
	Version:       spec.VersionOne,
	DisplayName:   "Apply unified diff",
	Description: "Apply or dry-run a unified diff against local UTF-8 text files. " +
		"Supports Git-style unified diffs, multi-file patches, path overrides, candidate local paths, " +
		"already-applied detection, no-newline-at-EOF markers, and safe fuzzy hunk matching. " +
		"The parser is intentionally tolerant of common malformed/LLM diffs. " +
		"Set dryRun=true to check without writing. Set strict=true to disable fuzzy matching.",
	Tags: []string{"fs", "text", "diff", "patch", "write"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"diffText": {
		"type": "string",
		"minLength": 1,
		"description": "Unified diff text to apply or dry-run. May contain one or more file patches and hunks."
	},
	"dryRun": {
		"type": "boolean",
		"default": false,
		"description": "If true, check applicability and return resolved fileTargets without writing files."
	},
	"strict": {
		"type": "boolean",
		"default": false,
		"description": "If true, use strict exact hunk matching only. Default false enables safe fuzzy matching."
	},
	"fileTargets": {
		"type": "array",
		"description": "Optional explicit target path mappings for file patches. Use when diff paths are missing, wrong, relative to a different base, or ambiguous.",
		"items": {
			"type": "object",
			"properties": {
				"fileKey": {
					"type": "string",
					"description": "Parsed file key such as file-1, file-2, etc."
				},
				"oldPath": {
					"type": "string",
					"description": "Old path from the diff, if known."
				},
				"newPath": {
					"type": "string",
					"description": "New path from the diff, if known."
				},
				"targetPath": {
					"type": "string",
					"minLength": 1,
					"description": "Actual local file path to use for this file patch."
				}
			},
			"required": ["targetPath"],
			"additionalProperties": false
		}
	},
	"candidatePaths": {
		"type": "array",
		"description": "Optional local paths to consider if paths parsed from the diff do not resolve. The tool matches by exact path, suffix, and unique basename.",
		"items": {
			"type": "string",
			"minLength": 1
		}
	}
},
"required": ["diffText"],
"additionalProperties": false
}`),

	GoImpl: spec.GoToolImpl{FuncID: applyUnifiedDiffFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type filePlanAction string

const (
	filePlanActionNoop          filePlanAction = "noop"
	filePlanActionCreate        filePlanAction = "create"
	filePlanActionWriteExisting filePlanAction = "write_existing"
	filePlanActionChmodExisting filePlanAction = "chmod_existing"
	filePlanActionDelete        filePlanAction = "delete"
)

type compareMode int

const (
	compareExact compareMode = iota
	compareTrimmed
)

var (
	hunkHeaderRE      = regexp.MustCompile(`^@@\s+-(\d+)(?:,(\d+))?\s+\+(\d+)(?:,(\d+))?\s+@@(?:\s?(.*))?$`)
	looseHunkHeaderRE = regexp.MustCompile(`^@@\s*-?(\d+)(?:,(\d+))?\s*\+?(\d+)(?:,(\d+))?\s*@@`)
)

type ApplyUnifiedDiffFileTarget struct {
	FileKey    string `json:"fileKey,omitempty"`
	OldPath    string `json:"oldPath,omitempty"`
	NewPath    string `json:"newPath,omitempty"`
	TargetPath string `json:"targetPath"`
}

type ApplyUnifiedDiffArgs struct {
	DiffText string `json:"diffText"`

	// Default false. If true, only checks and returns reusable FileTargets.
	DryRun bool `json:"dryRun,omitempty"`

	// Default false. If true, disables fuzzy matching.
	Strict bool `json:"strict,omitempty"`

	// Optional explicit path mappings.
	FileTargets []ApplyUnifiedDiffFileTarget `json:"fileTargets,omitempty"`

	// Optional local paths to consider when diff paths do not resolve.
	CandidatePaths []string `json:"candidatePaths,omitempty"`
}

type ApplyUnifiedDiffSummary struct {
	Files               int `json:"files"`
	Hunks               int `json:"hunks"`
	AppliedHunks        int `json:"appliedHunks"`
	AlreadyAppliedHunks int `json:"alreadyAppliedHunks"`
	AddedLines          int `json:"addedLines"`
	DeletedLines        int `json:"deletedLines"`
}

type ApplyUnifiedDiffFileOut struct {
	OK bool `json:"ok"`

	FileKey      string                 `json:"fileKey"`
	OldPath      string                 `json:"oldPath,omitempty"`
	NewPath      string                 `json:"newPath,omitempty"`
	TargetPath   string                 `json:"targetPath,omitempty"`
	ResolvedPath string                 `json:"resolvedPath,omitempty"`
	Status       ApplyUnifiedDiffStatus `json:"status"`
	Message      string                 `json:"message,omitempty"`

	CandidatePaths []string                     `json:"candidatePaths,omitempty"`
	Diagnostics    []ApplyUnifiedDiffDiagnostic `json:"diagnostics,omitempty"`

	Hunks               int `json:"hunks"`
	AppliedHunks        int `json:"appliedHunks"`
	AlreadyAppliedHunks int `json:"alreadyAppliedHunks"`
	AddedLines          int `json:"addedLines"`
	DeletedLines        int `json:"deletedLines"`
}

type ApplyUnifiedDiffOut struct {
	OK     bool                   `json:"ok"`
	DryRun bool                   `json:"dryRun"`
	Status ApplyUnifiedDiffStatus `json:"status"`

	Message     string                       `json:"message,omitempty"`
	Diagnostics []ApplyUnifiedDiffDiagnostic `json:"diagnostics,omitempty"`

	Summary ApplyUnifiedDiffSummary `json:"summary"`

	// Reusable as ApplyUnifiedDiffArgs.FileTargets.
	FileTargets []ApplyUnifiedDiffFileTarget `json:"fileTargets,omitempty"`

	Files []ApplyUnifiedDiffFileOut `json:"files,omitempty"`
}

type blockMatch struct {
	Start  int
	Method string
	Score  float64
}

type singleEditHunkSpec struct {
	Prefix  []string
	OldEdit []string
	NewEdit []string
	Suffix  []string
}

type singleEditMatch struct {
	EditStart      int
	EstimatedStart int
	Method         string
	AlreadyApplied bool
}

type hunkMatchBasis string

const (
	hunkMatchBasisNone   hunkMatchBasis = ""
	hunkMatchBasisOld    hunkMatchBasis = "old"
	hunkMatchBasisNew    hunkMatchBasis = "new"
	hunkMatchBasisInsert hunkMatchBasis = "insert"
)

type hunkApplyResult struct {
	Lines          []string
	OldLen         int
	NewLen         int
	AlreadyApplied bool
	Diagnostics    []ApplyUnifiedDiffDiagnostic

	Matched    bool
	MatchStart int
	MatchBasis hunkMatchBasis
}

type parsedHunkLine struct {
	Kind           byte
	Text           string
	NoNewlineAtEOF bool
}

type parsedHunk struct {
	Header   string
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Lines    []parsedHunkLine
}

type parsedPatchFile struct {
	FileKey        string
	FileKeyAliases []string
	OldPath        string
	NewPath        string
	Hunks          []parsedHunk

	AddedLines     int
	DeletedLines   int
	IsRename       bool
	IsCopy         bool
	IsNewFile      bool
	IsDeletedFile  bool
	HasBinaryPatch bool

	NewFilePerm       os.FileMode
	OldNoFinalNewline *bool
	NewNoFinalNewline *bool

	Diagnostics []ApplyUnifiedDiffDiagnostic
}

type parsedPatch struct {
	Files       []parsedPatchFile
	Diagnostics []ApplyUnifiedDiffDiagnostic
}

type looseHunkState struct {
	hunk              *parsedHunk
	oldSeen           int
	newSeen           int
	omittedPrefixDiag bool
	extraLinesDiag    bool
}

type fileApplyPlan struct {
	Result ApplyUnifiedDiffFileOut

	Action       filePlanAction
	DisplayPath  string
	ResolvedPath string
	Content      string
	Perm         os.FileMode

	VerifyContent   bool
	ExpectedContent string
}

type targetResolution struct {
	DisplayPath  string
	ResolvedPath string
}

type candidatePathInfo struct {
	Path         string
	ResolvedPath string
	Exists       bool
	IsDir        bool

	NormPath     string
	NormResolved string
	BasePath     string
	BaseResolved string
}

type createTargetInference struct {
	TargetPath    string
	SourcePath    string
	MatchedPrefix string
	Score         int
}

func (tt *TextTool) applyUnifiedDiff(ctx context.Context, args ApplyUnifiedDiffArgs) (*ApplyUnifiedDiffOut, error) {
	out := &ApplyUnifiedDiffOut{
		DryRun: args.DryRun,
		Status: ApplyUnifiedDiffStatusError,
	}

	if err := validateApplyUnifiedDiffArgs(args); err != nil {
		out.OK = false
		out.Status = ApplyUnifiedDiffStatusError
		out.Message = err.Error()
		out.Diagnostics = append(out.Diagnostics, errorDiagnostic("invalid_args", "%v", err))
		return out, nil
	}

	patch, err := parseUnifiedDiff(args.DiffText)
	if err != nil {
		out.OK = false
		out.Status = ApplyUnifiedDiffStatusError
		out.Message = err.Error()
		out.Diagnostics = append(out.Diagnostics, errorDiagnostic("parse_error", "%v", err))
		return out, nil
	}
	out.Diagnostics = append(out.Diagnostics, patch.Diagnostics...)

	if len(patch.Files) == 0 {
		out.OK = false
		out.Status = ApplyUnifiedDiffStatusNeedsInfo
		out.Message = "No file patches were found in the unified diff."
		out.Diagnostics = append(out.Diagnostics, warningDiagnostic("no_file_patches", "%s", out.Message))
		return out, nil
	}

	patch.Files = coalesceDuplicateParsedPatchFiles(patch.Files)
	p := tt.snapshotPolicy()
	candidateInfos := buildCandidatePathInfos(p, args.CandidatePaths)

	plans := make([]fileApplyPlan, 0, len(patch.Files))
	out.Files = make([]ApplyUnifiedDiffFileOut, 0, len(patch.Files))

	for i := range patch.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		plan := planFilePatch(ctx, p, patch.Files[i], args, candidateInfos, len(patch.Files) == 1)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
		out.Files = append(out.Files, plan.Result)
	}

	markDuplicateMutableTargets(plans, out.Files)

	out.Summary = summarizeApplyUnifiedDiffFiles(out.Files)
	out.FileTargets = reusableFileTargets(out.Files)
	out.OK, out.Status, out.Message = aggregatePlannedStatus(out.Files, args.DryRun)

	if args.DryRun || out.Status == ApplyUnifiedDiffStatusAlreadyApplied {
		return out, nil
	}

	if !hasExecutableFilePlans(plans, out.Files) {
		return out, nil
	}

	for i := range plans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if plans[i].Action == filePlanActionNoop || out.Files[i].Status != ApplyUnifiedDiffStatusApplicable {
			continue
		}

		if err := executeFilePlan(p, plans[i]); err != nil {
			msg := fmt.Sprintf("Failed to apply patch for %s: %v", plans[i].Result.FileKey, err)

			resetUnwrittenAppliedHunks(&out.Files[i], out.Files[i].AppliedHunks)
			out.Files[i].OK = false
			out.Files[i].Status = ApplyUnifiedDiffStatusError
			out.Files[i].Message = msg
			out.Files[i].Diagnostics = append(out.Files[i].Diagnostics, errorDiagnostic("write_failed", "%v", err))
			continue
		}

		if err := verifyExecutedFilePlan(p, plans[i]); err != nil {
			msg := fmt.Sprintf("Patch execution for %s could not be verified: %v", plans[i].Result.FileKey, err)
			resetUnverifiedAppliedHunks(&out.Files[i], out.Files[i].AppliedHunks)
			out.Files[i].OK = false
			out.Files[i].Status = ApplyUnifiedDiffStatusError
			out.Files[i].Message = msg
			out.Files[i].Diagnostics = append(
				out.Files[i].Diagnostics,
				errorDiagnostic("post_apply_verify_failed", "%v", err),
			)

			continue
		}

		out.Files[i].OK = true
		out.Files[i].Status = ApplyUnifiedDiffStatusApplied
		out.Files[i].Message = "Patch applied for this file."
	}

	out.Summary = summarizeApplyUnifiedDiffFiles(out.Files)
	out.FileTargets = reusableFileTargets(out.Files)
	out.OK, out.Status, out.Message = aggregateFinalStatus(out.Files)

	return out, nil
}

func validateApplyUnifiedDiffArgs(args ApplyUnifiedDiffArgs) error {
	if strings.TrimSpace(args.DiffText) == "" {
		return errors.New("diffText is required")
	}
	if len(args.DiffText) > hardUnifiedDiffBytes {
		return fmt.Errorf("diffText too large: %d bytes; max %d", len(args.DiffText), hardUnifiedDiffBytes)
	}
	if !utf8.ValidString(args.DiffText) {
		return errors.New("diffText is not valid UTF-8")
	}
	if len(args.FileTargets) > hardUnifiedDiffTargets {
		return fmt.Errorf("too many fileTargets: %d; max %d", len(args.FileTargets), hardUnifiedDiffTargets)
	}
	if len(args.CandidatePaths) > hardUnifiedDiffCandidates {
		return fmt.Errorf("too many candidatePaths: %d; max %d", len(args.CandidatePaths), hardUnifiedDiffCandidates)
	}

	seenFileKeys := map[string]int{}
	for i, target := range args.FileTargets {
		targetPath := strings.TrimSpace(target.TargetPath)
		if targetPath == "" {
			return fmt.Errorf("fileTargets[%d].targetPath is required", i)
		}
		if strings.ContainsRune(targetPath, 0) {
			return fmt.Errorf("fileTargets[%d].targetPath contains NUL byte", i)
		}
		if strings.ContainsRune(target.FileKey, 0) {
			return fmt.Errorf("fileTargets[%d].fileKey contains NUL byte", i)
		}
		if strings.ContainsRune(target.OldPath, 0) {
			return fmt.Errorf("fileTargets[%d].oldPath contains NUL byte", i)
		}
		if strings.ContainsRune(target.NewPath, 0) {
			return fmt.Errorf("fileTargets[%d].newPath contains NUL byte", i)
		}
		if key := strings.TrimSpace(target.FileKey); key != "" {
			if prev, ok := seenFileKeys[key]; ok {
				return fmt.Errorf("duplicate fileTargets fileKey %q at indexes %d and %d", key, prev, i)
			}
			seenFileKeys[key] = i
		}
	}

	for i, path := range args.CandidatePaths {
		path = strings.TrimSpace(path)
		if path == "" {
			return fmt.Errorf("candidatePaths[%d] must not be empty", i)
		}
		if strings.ContainsRune(path, 0) {
			return fmt.Errorf("candidatePaths[%d] contains NUL byte", i)
		}
	}

	return nil
}

func aggregatePlannedStatus(
	files []ApplyUnifiedDiffFileOut,
	dryRun bool,
) (ok bool, status ApplyUnifiedDiffStatus, msg string) {
	if len(files) == 0 {
		return false, ApplyUnifiedDiffStatusError, errNoFilesWereProcessed
	}

	hasNeedsInfo := false
	hasConflict := false
	hasError := false
	applicableFiles := 0
	alreadyAppliedFiles := 0
	appliedHunks := 0
	alreadyAppliedHunks := 0

	for _, f := range files {
		switch f.Status {
		case ApplyUnifiedDiffStatusApplicable, ApplyUnifiedDiffStatusApplied:
			applicableFiles++
		case ApplyUnifiedDiffStatusAlreadyApplied:
			alreadyAppliedFiles++
		case ApplyUnifiedDiffStatusNeedsInfo:
			hasNeedsInfo = true
		case ApplyUnifiedDiffStatusConflict:
			hasConflict = true
		case ApplyUnifiedDiffStatusError:
			hasError = true
		default:
		}

		appliedHunks += f.AppliedHunks
		alreadyAppliedHunks += f.AlreadyAppliedHunks
	}

	if hasNeedsInfo {
		return false, ApplyUnifiedDiffStatusNeedsInfo, "More target path information is required."
	}
	if hasError {
		return false, ApplyUnifiedDiffStatusError, "One or more files could not be checked."
	}
	if hasConflict {
		return false, ApplyUnifiedDiffStatusConflict, "One or more hunks could not be applied cleanly."
	}
	if appliedHunks == 0 && applicableFiles == 0 && alreadyAppliedFiles == len(files) {
		return true, ApplyUnifiedDiffStatusAlreadyApplied, "Unified diff is already applied."
	}

	if dryRun {
		return true, ApplyUnifiedDiffStatusApplicable, statusCanBeApplied
	}

	return true, ApplyUnifiedDiffStatusApplicable, statusCanBeApplied
}

func aggregateFinalStatus(files []ApplyUnifiedDiffFileOut) (ok bool, status ApplyUnifiedDiffStatus, msg string) {
	if len(files) == 0 {
		return false, ApplyUnifiedDiffStatusError, errNoFilesWereProcessed
	}

	hasNeedsInfo := false
	hasConflict := false
	hasError := false
	appliedFiles := 0
	applicableFiles := 0
	alreadyAppliedFiles := 0

	for _, f := range files {
		switch f.Status {
		case ApplyUnifiedDiffStatusApplied:
			appliedFiles++
		case ApplyUnifiedDiffStatusApplicable:
			applicableFiles++
		case ApplyUnifiedDiffStatusAlreadyApplied:
			alreadyAppliedFiles++
		case ApplyUnifiedDiffStatusNeedsInfo:
			hasNeedsInfo = true
		case ApplyUnifiedDiffStatusConflict:
			hasConflict = true
		case ApplyUnifiedDiffStatusError:
			hasError = true
		default:
		}
	}

	if hasError || hasConflict || hasNeedsInfo {
		failureStatus := ApplyUnifiedDiffStatusNeedsInfo
		message := "More target path information is required."

		if hasConflict {
			failureStatus = ApplyUnifiedDiffStatusConflict
			message = "One or more hunks could not be applied cleanly."
		}
		if hasError {
			failureStatus = ApplyUnifiedDiffStatusError
			message = "One or more files could not be checked or written."
		}

		if appliedFiles > 0 {
			return false, failureStatus, "Unified diff was partially applied; " + message
		}
		return false, failureStatus, message
	}

	if appliedFiles > 0 {
		return true, ApplyUnifiedDiffStatusApplied, "Unified diff applied successfully."
	}
	if applicableFiles > 0 {
		return true, ApplyUnifiedDiffStatusApplicable, statusCanBeApplied
	}
	if alreadyAppliedFiles > 0 {
		return true, ApplyUnifiedDiffStatusAlreadyApplied, "Unified diff was already applied."
	}

	return false, ApplyUnifiedDiffStatusError, "No file patches were applied."
}

func reusableFileTargets(files []ApplyUnifiedDiffFileOut) []ApplyUnifiedDiffFileTarget {
	out := make([]ApplyUnifiedDiffFileTarget, 0, len(files))

	for _, file := range files {
		if strings.TrimSpace(file.TargetPath) == "" {
			continue
		}

		out = append(out, ApplyUnifiedDiffFileTarget{
			FileKey:    file.FileKey,
			OldPath:    file.OldPath,
			NewPath:    file.NewPath,
			TargetPath: file.TargetPath,
		})
	}

	return out
}

func summarizeApplyUnifiedDiffFiles(files []ApplyUnifiedDiffFileOut) ApplyUnifiedDiffSummary {
	var out ApplyUnifiedDiffSummary
	for _, file := range files {
		out.Files++
		out.Hunks += file.Hunks
		out.AppliedHunks += file.AppliedHunks
		out.AlreadyAppliedHunks += file.AlreadyAppliedHunks
		out.AddedLines += file.AddedLines
		out.DeletedLines += file.DeletedLines
	}
	return out
}

func hasExecutableFilePlans(plans []fileApplyPlan, files []ApplyUnifiedDiffFileOut) bool {
	for i := range plans {
		if i >= len(files) {
			return false
		}
		if plans[i].Action != filePlanActionNoop && files[i].Status == ApplyUnifiedDiffStatusApplicable {
			return true
		}
	}
	return false
}

func markDuplicateMutableTargets(plans []fileApplyPlan, files []ApplyUnifiedDiffFileOut) {
	if len(plans) != len(files) {
		return
	}

	seen := map[string]int{}
	for i := range plans {
		if !plans[i].Action.mutates() || strings.TrimSpace(plans[i].ResolvedPath) == "" {
			continue
		}

		key := normalizePathForCompare(plans[i].ResolvedPath)
		if first, ok := seen[key]; ok {
			markDuplicateTargetConflict(plans, files, first, i)
			continue
		}
		seen[key] = i
	}
}

func markDuplicateTargetConflict(
	plans []fileApplyPlan,
	files []ApplyUnifiedDiffFileOut,
	first int,
	second int,
) {
	target := plans[second].DisplayPath
	if target == "" {
		target = plans[second].ResolvedPath
	}

	msg := fmt.Sprintf(
		"multiple file patches resolve to the same target path %q; combine hunks into one file patch or provide distinct fileTargets",
		target,
	)

	for _, idx := range []int{first, second} {
		if idx < 0 || idx >= len(plans) || idx >= len(files) {
			continue
		}

		other := first
		if idx == first {
			other = second
		}

		plans[idx].Action = filePlanActionNoop
		files[idx].OK = false
		files[idx].Status = ApplyUnifiedDiffStatusConflict
		files[idx].Message = msg
		files[idx].AppliedHunks = 0
		files[idx].AlreadyAppliedHunks = 0
		if other >= 0 && other < len(files) {
			files[idx].Diagnostics = append(
				files[idx].Diagnostics,
				errorDiagnostic("duplicate_target", "duplicate target also used by %s", files[other].FileKey),
			)
		}
		plans[idx].Result = files[idx]
	}
}

func parseUnifiedDiff(diffText string) (parsedPatch, error) {
	text := strings.ReplaceAll(diffText, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.TrimPrefix(text, "\ufeff")
	diffEndsWithNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(text, "\n")

	var out parsedPatch
	var current *parsedPatchFile
	var currentHunk *parsedHunk
	var hunkState looseHunkState

	addPatchDiag := func(level ApplyUnifiedDiffDiagnosticLevel, code, format string, args ...any) {
		if len(out.Diagnostics) >= hardUnifiedDiffParserDiagnostics {
			return
		}
		out.Diagnostics = append(out.Diagnostics, newApplyUnifiedDiffDiagnostic(level, code, format, args...))
	}

	addFileDiag := func(level ApplyUnifiedDiffDiagnosticLevel, code, format string, args ...any) {
		if current == nil || len(current.Diagnostics) >= hardUnifiedDiffParserDiagnostics {
			return
		}
		current.Diagnostics = append(current.Diagnostics, newApplyUnifiedDiffDiagnostic(level, code, format, args...))
	}

	addFileDiagnostic := func(diagnostic ApplyUnifiedDiffDiagnostic) {
		if current == nil || len(current.Diagnostics) >= hardUnifiedDiffParserDiagnostics {
			return
		}
		current.Diagnostics = append(current.Diagnostics, diagnostic)
	}

	closeCurrentHunk := func() {
		currentHunk = nil
		hunkState = looseHunkState{}
	}

	finishCurrentHunk := func() {
		if currentHunk != nil && hunkState.countsKnown() && !hunkState.declaredComplete() {
			actualOld, actualNew := currentHunk.bodyCounts()
			addFileDiag(
				ApplyUnifiedDiffDiagnosticLevelWarning,
				"hunk_header_count_mismatch",
				"hunk %s header counts differ from parsed body; using parsed body and treating header counts as hints only: declared -%d/+%d, parsed -%d/+%d",
				currentHunk.Header,
				currentHunk.OldCount,
				currentHunk.NewCount,
				actualOld,
				actualNew,
			)
		}
		closeCurrentHunk()
	}

	pushCurrent := func() {
		finishCurrentHunk()
		if current == nil {
			return
		}

		if current.OldPath != "" || current.NewPath != "" || len(current.Hunks) > 0 {
			out.Files = append(out.Files, *current)
		}

		current = nil
	}

	newFile := func() *parsedPatchFile {
		return &parsedPatchFile{
			FileKey: fmt.Sprintf("file-%d", len(out.Files)+1),
		}
	}

	ensureCurrent := func() *parsedPatchFile {
		if current == nil {
			current = newFile()
		}
		return current
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		if currentHunk != nil {
			if isNoNewlineAtEOFMarker(line) {

				markNoNewlineAtEndOfFile(current, currentHunk)
				continue
			}

			if isMarkdownFenceLine(line) && (hunkState.declaredComplete() || !hasHunkBodyPrefix(line)) {
				finishCurrentHunk()
				continue
			}
			if diffEndsWithNewline && line == "" && i == len(lines)-1 {
				finishCurrentHunk()
				continue
			}

			if isPatchBoundary(lines, i) ||
				(hunkState.declaredComplete() && isLikelyPatchMetadataLine(line)) ||
				(hunkState.declaredComplete() && !hasHunkBodyPrefix(line)) {
				finishCurrentHunk()
				i--
				continue
			}

			parsedLine, oldInc, newInc, hadPrefix := parseHunkBodyLine(line)
			if !hadPrefix && !hunkState.omittedPrefixDiag {
				addFileDiag(
					ApplyUnifiedDiffDiagnosticLevelWarning,
					"hunk_body_prefix_missing",
					"hunk %s contains lines without unified-diff prefixes; treating them as context",
					currentHunk.Header,
				)
				hunkState.omittedPrefixDiag = true
			}
			if hunkState.wouldOverflow(oldInc, newInc) && !hunkState.extraLinesDiag {
				addFileDiag(
					ApplyUnifiedDiffDiagnosticLevelWarning,
					"hunk_body_overflow",
					"hunk %s contains more body lines than its header declares; treating extra lines as part of the hunk",
					currentHunk.Header,
				)
				hunkState.extraLinesDiag = true
			}

			currentHunk.Lines = append(currentHunk.Lines, parsedLine)
			hunkState.oldSeen += oldInc
			hunkState.newSeen += newInc

			if current != nil {
				switch parsedLine.Kind {
				case '+':
					current.AddedLines++
				case '-':
					current.DeletedLines++
				}
			}

			continue
		}
		if strings.HasPrefix(line, "Index: ") {
			pushCurrent()
			current = newFile()
			p := parseDiffPathToken(strings.TrimSpace(strings.TrimPrefix(line, "Index: ")))
			current.OldPath = p
			current.NewPath = p
			continue
		}
		if strings.HasPrefix(line, "diff --git ") {
			pushCurrent()
			current = newFile()
			oldPath, newPath := parseGitDiffPaths(strings.TrimSpace(strings.TrimPrefix(line, "diff --git ")))
			current.OldPath = oldPath
			current.NewPath = newPath
			continue
		}

		if current != nil {
			switch {
			case strings.HasPrefix(line, "new file mode "):
				current.IsNewFile = true
				if current.OldPath == "" || patchPathsEqual(current.OldPath, current.NewPath) {
					current.OldPath = pathDevNull
				}
				if perm, ok := parseGitFileModePerm(line); ok {
					current.NewFilePerm = perm
				}
				continue
			case strings.HasPrefix(line, "deleted file mode "):
				current.IsDeletedFile = true
				if current.NewPath == "" || patchPathsEqual(current.NewPath, current.OldPath) {
					current.NewPath = pathDevNull
				}
				continue
			case strings.HasPrefix(line, "new mode "):
				if perm, ok := parseGitFileModePerm(line); ok {
					current.NewFilePerm = perm
				}
				continue
			case strings.HasPrefix(line, "rename from "):
				current.IsRename = true

				p := parseDiffPathToken(strings.TrimSpace(strings.TrimPrefix(line, "rename from ")))
				if p != "" && current.OldPath == "" {
					current.OldPath = p
				}
				continue
			case strings.HasPrefix(line, "rename to "):
				current.IsRename = true
				p := parseDiffPathToken(strings.TrimSpace(strings.TrimPrefix(line, "rename to ")))
				if p != "" && current.NewPath == "" {
					current.NewPath = p
				}
				continue
			case strings.HasPrefix(line, "copy from "):
				current.IsCopy = true
				p := parseDiffPathToken(strings.TrimSpace(strings.TrimPrefix(line, "copy from ")))
				if p != "" && current.OldPath == "" {
					current.OldPath = p
				}
				continue
			case strings.HasPrefix(line, "copy to "):
				current.IsCopy = true
				p := parseDiffPathToken(strings.TrimSpace(strings.TrimPrefix(line, "copy to ")))
				if p != "" && current.NewPath == "" {
					current.NewPath = p
				}
				continue
			case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
				current.HasBinaryPatch = true
				addFileDiag(
					ApplyUnifiedDiffDiagnosticLevelWarning,
					"binary_patch_ignored",
					"binary patch content is ignored; this tool only applies UTF-8 text hunks",
				)
				continue
			}
		}

		if isPlainUnifiedFileHeader(lines, i) {
			if current == nil || len(current.Hunks) > 0 {
				pushCurrent()
				current = newFile()
			}

			oldPath := parseDiffPathToken(strings.TrimSpace(strings.TrimPrefix(lines[i], "--- ")))
			newPath := parseDiffPathToken(strings.TrimSpace(strings.TrimPrefix(lines[i+1], "+++ ")))
			current.OldPath, current.NewPath = normalizeUnifiedHeaderPathPair(oldPath, newPath)
			closeCurrentHunk()
			i++
			continue
		}

		if strings.HasPrefix(line, "@@") {
			f := ensureCurrent()
			hunk, diag, hasDiag := parseHunkHeader(line)
			if hasDiag {
				addFileDiagnostic(diag)
			}

			f.Hunks = append(f.Hunks, hunk)
			currentHunk = &f.Hunks[len(f.Hunks)-1]
			hunkState = looseHunkState{hunk: currentHunk}
			continue
		}
	}

	pushCurrent()

	if len(out.Files) > hardUnifiedDiffFiles {
		return parsedPatch{}, fmt.Errorf("too many files in diff: %d; max %d", len(out.Files), hardUnifiedDiffFiles)
	}

	totalHunks := 0
	for _, file := range out.Files {
		totalHunks += len(file.Hunks)
	}
	if totalHunks > hardUnifiedDiffHunks {
		return parsedPatch{}, fmt.Errorf("too many hunks in diff: %d; max %d", totalHunks, hardUnifiedDiffHunks)
	}

	if len(out.Diagnostics) == 0 && len(out.Files) == 0 {
		addPatchDiag(
			ApplyUnifiedDiffDiagnosticLevelWarning,
			"no_patch_headers_found",
			"no recognizable unified-diff file or hunk headers were found",
		)
	}

	return out, nil
}

func coalesceDuplicateParsedPatchFiles(files []parsedPatchFile) []parsedPatchFile {
	if len(files) <= 1 {
		return files
	}

	out := make([]parsedPatchFile, 0, len(files))
	seen := map[string]int{}

	for _, file := range files {
		key, ok := parsedPatchFileCoalesceKey(file)
		if !ok {
			out = append(out, file)
			continue
		}

		if idx, exists := seen[key]; exists {
			mergeParsedPatchFile(&out[idx], file)
			continue
		}

		seen[key] = len(out)
		out = append(out, file)
	}

	return out
}

func parsedPatchFileCoalesceKey(file parsedPatchFile) (string, bool) {
	if file.IsRename || file.IsCopy {
		return "", false
	}

	paths := patchPathCandidates(file)
	if len(paths) == 0 {
		return "", false
	}

	kind := "edit"
	if file.isDelete() {
		kind = "delete"
	}

	norm := normalizePathForCompare(paths[0])
	if norm == "" || isDevNull(norm) {
		return "", false
	}

	return kind + ":" + norm, true
}

func mergeParsedPatchFile(dst *parsedPatchFile, src parsedPatchFile) {
	if dst == nil {
		return
	}

	target := ""
	if paths := patchPathCandidates(src); len(paths) > 0 {
		target = paths[0]
	}

	dst.FileKeyAliases = appendUniqueTrimmedStrings(dst.FileKeyAliases, src.FileKey)
	dst.FileKeyAliases = appendUniqueTrimmedStrings(dst.FileKeyAliases, src.FileKeyAliases...)

	dst.Hunks = append(dst.Hunks, src.Hunks...)
	dst.AddedLines += src.AddedLines
	dst.DeletedLines += src.DeletedLines
	dst.IsRename = dst.IsRename || src.IsRename
	dst.IsCopy = dst.IsCopy || src.IsCopy
	dst.IsNewFile = dst.IsNewFile || src.IsNewFile
	dst.IsDeletedFile = dst.IsDeletedFile || src.IsDeletedFile
	dst.HasBinaryPatch = dst.HasBinaryPatch || src.HasBinaryPatch

	if dst.OldPath == "" {
		dst.OldPath = src.OldPath
	}
	if dst.NewPath == "" {
		dst.NewPath = src.NewPath
	}

	if src.NewFilePerm != 0 {
		if dst.NewFilePerm != 0 && dst.NewFilePerm != src.NewFilePerm {
			dst.Diagnostics = append(
				dst.Diagnostics,
				warningDiagnostic(
					"duplicate_file_mode",
					"duplicate file patches specify different new modes %#o and %#o; using %#o",
					dst.NewFilePerm,
					src.NewFilePerm,
					src.NewFilePerm,
				),
			)
		}
		dst.NewFilePerm = src.NewFilePerm
	}

	if src.OldNoFinalNewline != nil {
		dst.OldNoFinalNewline = src.OldNoFinalNewline
	}
	if src.NewNoFinalNewline != nil {
		dst.NewNoFinalNewline = src.NewNoFinalNewline
	}

	dst.Diagnostics = append(dst.Diagnostics, src.Diagnostics...)
	if target != "" {
		dst.Diagnostics = append(
			dst.Diagnostics,
			infoDiagnostic("merged_duplicate_file_patch",
				"merged duplicate file patch %s into %s for target %q", src.FileKey, dst.FileKey, target),
		)
	} else {
		dst.Diagnostics = append(
			dst.Diagnostics,
			infoDiagnostic("merged_duplicate_file_patch",
				"merged duplicate file patch %s into %s", src.FileKey, dst.FileKey),
		)
	}
}

func appendUniqueTrimmedStrings(in []string, values ...string) []string {
	out := make([]string, 0, len(in)+len(values))
	seen := map[string]bool{}

	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}

	for _, s := range values {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}

	return out
}

func isPatchBoundary(lines []string, i int) bool {
	if i < 0 || i >= len(lines) {
		return false
	}
	line := lines[i]
	return strings.HasPrefix(line, "diff --git ") ||
		isPlainUnifiedFileHeader(lines, i) ||
		strings.HasPrefix(line, "@@")
}

func isLikelyPatchMetadataLine(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}

	prefixes := []string{
		"index ",
		"old mode ",
		"new mode ",
		"deleted file mode ",
		"new file mode ",
		"similarity index ",
		"dissimilarity index ",
		"rename from ",
		"rename to ",
		"copy from ",
		"copy to ",
		"Binary files ",
		"GIT binary patch",
		"literal ",
		"delta ",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func hasHunkBodyPrefix(line string) bool {
	if line == "" {
		return false
	}
	switch line[0] {
	case ' ', '+', '-':
		return true
	default:
		return false
	}
}

func parseHunkBodyLine(line string) (parsedLine parsedHunkLine, oldInc, newInc int, hadPrefix bool) {
	kind := byte(' ')
	text := line

	if line != "" {
		switch line[0] {
		case ' ', '+', '-':
			kind = line[0]
			text = line[1:]
			hadPrefix = true
		default:
			kind = ' '
			text = line
		}
	}

	oldInc, newInc = hunkLineCounts(kind)
	return parsedHunkLine{Kind: kind, Text: text}, oldInc, newInc, hadPrefix
}

func hunkLineCounts(kind byte) (oldInc, newInc int) {
	switch kind {
	case '+':
		return 0, 1
	case '-':
		return 1, 0
	default:
		return 1, 1
	}
}

func markNoNewlineAtEndOfFile(file *parsedPatchFile, hunk *parsedHunk) {
	if hunk == nil || len(hunk.Lines) == 0 {
		return
	}

	idx := len(hunk.Lines) - 1
	hunk.Lines[idx].NoNewlineAtEOF = true
	noFinal := true

	if file == nil {
		return
	}

	switch hunk.Lines[idx].Kind {
	case '+':
		file.NewNoFinalNewline = &noFinal
	case '-':
		file.OldNoFinalNewline = &noFinal
	default:
		file.OldNoFinalNewline = &noFinal
		file.NewNoFinalNewline = &noFinal
	}
}

func isPlainUnifiedFileHeader(lines []string, i int) bool {
	if i < 0 || i+1 >= len(lines) {
		return false
	}
	return strings.HasPrefix(lines[i], "--- ") && strings.HasPrefix(lines[i+1], "+++ ")
}

func parseGitDiffPaths(rest string) (oldPath, newPath string) {
	first, remaining := readDiffPathToken(rest)
	second, _ := readDiffPathToken(remaining)
	return stripGitPathPrefix(normalizeParsedDiffPathToken(first), "a"),
		stripGitPathPrefix(normalizeParsedDiffPathToken(second), "b")
}

func parseDiffPathToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}

	if strings.HasPrefix(token, `"`) {
		token, _ = readDiffPathToken(token)
	} else {
		if idx := strings.IndexByte(token, '\t'); idx >= 0 {
			token = token[:idx]
		} else {
			fields := strings.Fields(token)
			if len(fields) > 0 {
				token = fields[0]
			}
		}
	}

	return normalizeParsedDiffPathToken(token)
}

func normalizeUnifiedHeaderPathPair(oldPath, newPath string) (outOldPath, outNewPath string) {
	outOldPath = normalizeParsedDiffPathToken(oldPath)
	outNewPath = normalizeParsedDiffPathToken(newPath)

	oldHasGitPrefix := strings.HasPrefix(outOldPath, "a/")
	newHasGitPrefix := strings.HasPrefix(outNewPath, "b/")

	// Strip git's synthetic a/ and b/ prefixes only when the header pair looks
	// like a git-style pair. Do not strip arbitrary plain unified paths such as
	// "a/foo" -> "a/foo".
	if (oldHasGitPrefix && (newHasGitPrefix || isDevNull(outNewPath))) ||
		(newHasGitPrefix && isDevNull(outOldPath)) {
		outOldPath = stripGitPathPrefix(outOldPath, "a")
		outNewPath = stripGitPathPrefix(outNewPath, "b")
	}

	return outOldPath, outNewPath
}

func normalizeParsedDiffPathToken(token string) string {
	token = strings.TrimSpace(token)
	if token == pathDevNull {
		return token
	}

	return strings.TrimSpace(token)
}

func stripGitPathPrefix(token, prefix string) string {
	token = strings.TrimSpace(token)
	if token == "" || isDevNull(token) {
		return token
	}

	marker := prefix + "/"
	if strings.HasPrefix(token, marker) {
		return strings.TrimSpace(token[len(marker):])
	}
	return token
}

func readDiffPathToken(s string) (first, remaining string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}

	if s[0] != '"' {
		for i := 0; i < len(s); i++ {
			if s[i] == ' ' || s[i] == '\t' {
				return s[:i], strings.TrimSpace(s[i:])
			}
		}
		return s, ""
	}

	escaped := false
	for i := 1; i < len(s); i++ {
		ch := s[i]

		if escaped {
			escaped = false
			continue
		}

		if ch == '\\' {
			escaped = true
			continue
		}

		if ch == '"' {
			raw := s[:i+1]
			if unquoted, err := strconv.Unquote(raw); err == nil {
				return unquoted, strings.TrimSpace(s[i+1:])
			}
			return fallbackUnquoteDiffPath(raw), strings.TrimSpace(s[i+1:])
		}
	}

	return fallbackUnquoteDiffPath(s), ""
}

func fallbackUnquoteDiffPath(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, `"`)
	raw = strings.TrimSuffix(raw, `"`)

	var b strings.Builder
	escaped := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if escaped {
			b.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		b.WriteByte(ch)
	}
	if escaped {
		b.WriteByte('\\')
	}
	return b.String()
}

func parseHunkHeader(line string) (parsedHunk, ApplyUnifiedDiffDiagnostic, bool) {
	if m := hunkHeaderRE.FindStringSubmatch(line); m != nil {
		return parsedHunk{
			Header:   line,
			OldStart: atoiDefault(m[1], 0),
			OldCount: atoiDefault(m[2], 1),
			NewStart: atoiDefault(m[3], 0),
			NewCount: atoiDefault(m[4], 1),
		}, ApplyUnifiedDiffDiagnostic{}, false
	}

	if m := looseHunkHeaderRE.FindStringSubmatch(line); m != nil {
		p := parsedHunk{
			Header:   line,
			OldStart: atoiDefault(m[1], 0),
			OldCount: atoiDefault(m[2], 1),
			NewStart: atoiDefault(m[3], 0),
			NewCount: atoiDefault(m[4], 1),
		}
		return p, warningDiagnostic(
			"non_standard_hunk_header",
			"non-standard hunk header %q parsed with loose rules",
			line,
		), true
	}

	p := parsedHunk{
		Header:   line,
		OldStart: 0,
		OldCount: -1,
		NewStart: 0,
		NewCount: -1,
	}
	return p, warningDiagnostic(
		"malformed_hunk_header",
		"malformed hunk header %q; line-number hints unavailable and body will be parsed until the next patch boundary",
		line,
	), true
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func parseGitFileModePerm(line string) (os.FileMode, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, false
	}

	n, err := strconv.ParseUint(fields[len(fields)-1], 8, 32)
	if err != nil {
		return 0, false
	}

	perm := os.FileMode(n) & os.ModePerm
	if perm == 0 {
		return 0, false
	}
	return perm, true
}

func planFilePatch(
	ctx context.Context,
	p fspolicy.FSPolicy,
	file parsedPatchFile,
	args ApplyUnifiedDiffArgs,
	candidateInfos []candidatePathInfo,
	isOnlyPatchFile bool,
) fileApplyPlan {
	result := ApplyUnifiedDiffFileOut{
		OK:           false,
		FileKey:      file.FileKey,
		OldPath:      file.OldPath,
		NewPath:      file.NewPath,
		Status:       ApplyUnifiedDiffStatusConflict,
		Hunks:        len(file.Hunks),
		AddedLines:   file.AddedLines,
		DeletedLines: file.DeletedLines,
		Diagnostics:  cloneApplyUnifiedDiffDiagnostics(file.Diagnostics),
	}
	if file.IsCopy {

		result.Status = ApplyUnifiedDiffStatusConflict
		result.Message = "git copy patches are not safely supported by applyUnifiedDiff yet"

		result.Diagnostics = append(
			result.Diagnostics,
			errorDiagnostic(
				"copy_unsupported",
				"refusing to apply copy metadata as a plain edit because that can modify the source path instead of creating the copied target",
			),
		)
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}
	if file.IsRename {
		result.Diagnostics = append(
			result.Diagnostics,
			warningDiagnostic(
				"rename_metadata_ignored",
				"git rename metadata is ignored; text hunks will be applied to an existing target path and no filesystem rename will be performed",
			),
		)
		if len(file.Hunks) == 0 && file.NewFilePerm == 0 {
			result.Status = ApplyUnifiedDiffStatusConflict
			result.Message = "rename-only patches are not applied because applyUnifiedDiff does not rename files"
			result.Diagnostics = append(
				result.Diagnostics,
				errorDiagnostic(
					"rename_without_text_hunks_unsupported",
					"rename patch has no text hunks or mode change to apply after ignoring rename metadata",
				),
			)
			return fileApplyPlan{Result: result, Action: filePlanActionNoop}
		}
	}

	if file.HasBinaryPatch {
		result.Status = ApplyUnifiedDiffStatusConflict
		result.Message = "binary patches are not supported by applyUnifiedDiff"
		result.Diagnostics = append(
			result.Diagnostics,
			errorDiagnostic(
				"binary_patch_unsupported",
				"refusing to treat ignored binary patch content as already applied",
			),
		)
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}
	modeOnly := file.isModeOnly()
	if len(file.Hunks) == 0 && !modeOnly && !file.isCreate() && !file.isDelete() {
		result.OK = true
		result.Status = ApplyUnifiedDiffStatusAlreadyApplied
		result.Message = "File patch contains no text hunks; nothing to apply."
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	target, candidates, diagnostics, status, err := resolvePatchTarget(p, file, args, candidateInfos, isOnlyPatchFile)
	result.CandidatePaths = candidates
	result.Diagnostics = append(result.Diagnostics, diagnostics...)

	if target.DisplayPath != "" {
		result.TargetPath = target.DisplayPath
	}
	if target.ResolvedPath != "" {
		result.ResolvedPath = target.ResolvedPath
	}

	if err != nil {

		result.Status = status
		result.Message = err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	exists, err := resolvedPathExists(target.ResolvedPath)
	if err != nil {
		result.Status = ApplyUnifiedDiffStatusError
		result.Message = err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}
	if modeOnly {
		return planModeOnlyFilePatch(p, file, target, result, exists)
	}
	if file.isCreateLike() {
		return planCreateFilePatch(ctx, p, file, args, target, result, exists)
	}

	if !exists && file.canCreateWhenMissing() {
		result.Diagnostics = append(
			result.Diagnostics,
			infoDiagnostic(
				"missing_target_treated_as_create",
				"target does not exist and diff looks like a new-file patch; treating it as a create",
			),
		)
		return planCreateFilePatch(ctx, p, file, args, target, result, exists)
	}

	if file.isDelete() && !exists {
		result.OK = true
		result.Status = ApplyUnifiedDiffStatusAlreadyApplied
		result.Message = "Delete patch is already applied because the target file does not exist."
		result.AlreadyAppliedHunks = len(file.Hunks)
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	if !exists {
		result.Status = ApplyUnifiedDiffStatusNeedsInfo
		result.Message = "target file does not exist: " + target.DisplayPath
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	if err := requireNoSymlinkExistingRegularFileResolved(p, target.ResolvedPath); err != nil {
		result.Status = ApplyUnifiedDiffStatusError
		result.Message = err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	tf, err := readTextFileNoSymlink(p, target.ResolvedPath)
	if err != nil {
		result.Status = ApplyUnifiedDiffStatusError
		result.Message = err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}
	originalContent := tf.Render()
	targetPerm := tf.Perm
	hasModeChange := false

	if file.NewFilePerm != 0 {
		targetPerm = file.NewFilePerm
		hasModeChange = targetPerm != tf.Perm
	}
	hunksToApply := file.Hunks
	if file.isDelete() {
		var normalized bool
		hunksToApply, normalized = normalizeDeleteFileHunks(file.Hunks)
		if normalized {
			result.Diagnostics = append(
				result.Diagnostics,
				warningDiagnostic(
					"delete_context_treated_as_deleted",
					"delete-file patch contains context lines; treating them as old-side lines because the new path is /dev/null",
				),
			)
		}
	}
	nextLines, applied, already, hunkDiagnostics, err := applyPatchHunks(
		ctx,
		tf.Lines,
		hunksToApply,
		!args.Strict,
	)
	result.Diagnostics = append(result.Diagnostics, hunkDiagnostics...)
	result.AppliedHunks = applied
	result.AlreadyAppliedHunks = already

	if err != nil {
		resetUnwrittenAppliedHunks(&result, applied)
		result.Status = ApplyUnifiedDiffStatusConflict
		result.Message = err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	if file.isDelete() {
		if len(nextLines) != 0 {
			resetUnwrittenAppliedHunks(&result, applied)

			result.Status = ApplyUnifiedDiffStatusConflict
			result.Message = "delete-file patch did not leave the target file empty"
			return fileApplyPlan{Result: result, Action: filePlanActionNoop}
		}

		result.OK = true
		result.Status = ApplyUnifiedDiffStatusApplicable
		result.Message = "File can be deleted by this patch."

		return fileApplyPlan{
			Result:          result,
			Action:          filePlanActionDelete,
			DisplayPath:     target.DisplayPath,
			ResolvedPath:    target.ResolvedPath,
			VerifyContent:   true,
			ExpectedContent: originalContent,
		}
	}

	if applied == 0 && already > 0 && !hasModeChange {

		result.OK = true
		result.Status = ApplyUnifiedDiffStatusAlreadyApplied
		result.Message = "Patch is already applied for this file."
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	originalLineCount := len(tf.Lines)
	tf.Lines = nextLines
	tf.HasFinalNewline = patchedFileHasFinalNewline(tf.HasFinalNewline, originalLineCount, file, nextLines)
	content := tf.Render()

	if content == originalContent && !hasModeChange {

		result.OK = true
		result.Status = ApplyUnifiedDiffStatusAlreadyApplied
		result.Message = "Patch is already applied for this file."
		result.AlreadyAppliedHunks += result.AppliedHunks
		result.AppliedHunks = 0
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}
	if content == originalContent && hasModeChange {
		result.Diagnostics = append(
			result.Diagnostics,
			infoDiagnostic(
				"mode_change_only",
				"file content already matches; file mode can be updated from %#o to %#o",
				tf.Perm, targetPerm,
			),
		)
		result.AlreadyAppliedHunks += result.AppliedHunks
		result.AppliedHunks = 0
	}
	if err := validateUnifiedDiffOutputContent(content); err != nil {
		result.Status = ApplyUnifiedDiffStatusError
		result.Message = err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	result.OK = true
	result.Status = ApplyUnifiedDiffStatusApplicable
	result.Message = "Patch can be applied for this file."

	return fileApplyPlan{
		Result:          result,
		Action:          filePlanActionWriteExisting,
		DisplayPath:     target.DisplayPath,
		ResolvedPath:    target.ResolvedPath,
		Content:         content,
		Perm:            targetPerm,
		VerifyContent:   true,
		ExpectedContent: originalContent,
	}
}

func normalizeDeleteFileHunks(hunks []parsedHunk) ([]parsedHunk, bool) {
	out := make([]parsedHunk, len(hunks))
	changed := false

	for i, hunk := range hunks {
		out[i] = hunk
		out[i].Lines = make([]parsedHunkLine, len(hunk.Lines))

		for j, line := range hunk.Lines {
			if line.Kind == ' ' {
				line.Kind = '-'
				changed = true
			}
			out[i].Lines[j] = line
		}

		if changed {
			oldCount, newCount := out[i].bodyCounts()
			out[i].OldCount = oldCount
			out[i].NewCount = newCount
		}
	}

	return out, changed
}

func planModeOnlyFilePatch(
	p fspolicy.FSPolicy,
	file parsedPatchFile,
	target targetResolution,
	result ApplyUnifiedDiffFileOut,
	exists bool,
) fileApplyPlan {
	if !exists {
		result.Status = ApplyUnifiedDiffStatusNeedsInfo
		result.Message = "target file does not exist: " + target.DisplayPath
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	if err := requireNoSymlinkExistingRegularFileResolved(p, target.ResolvedPath); err != nil {
		result.Status = ApplyUnifiedDiffStatusError
		result.Message = err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	tf, err := readTextFileNoSymlink(p, target.ResolvedPath)
	if err != nil {
		result.Status = ApplyUnifiedDiffStatusError
		result.Message = err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	targetPerm := file.NewFilePerm
	if targetPerm == 0 {
		result.Status = ApplyUnifiedDiffStatusAlreadyApplied
		result.OK = true
		result.Message = "File patch contains no text hunks; nothing to apply."
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	if tf.Perm == targetPerm {
		result.OK = true
		result.Status = ApplyUnifiedDiffStatusAlreadyApplied
		result.Message = "Mode-only patch is already applied for this file."
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	result.OK = true
	result.Status = ApplyUnifiedDiffStatusApplicable
	result.Message = fmt.Sprintf("File mode can be updated from %#o to %#o.", tf.Perm, targetPerm)

	return fileApplyPlan{
		Result:          result,
		Action:          filePlanActionChmodExisting,
		DisplayPath:     target.DisplayPath,
		ResolvedPath:    target.ResolvedPath,
		Perm:            targetPerm,
		VerifyContent:   true,
		ExpectedContent: tf.Render(),
	}
}

func planCreateFilePatch(
	ctx context.Context,
	p fspolicy.FSPolicy,
	file parsedPatchFile,
	args ApplyUnifiedDiffArgs,
	target targetResolution,
	result ApplyUnifiedDiffFileOut,
	exists bool,
) fileApplyPlan {
	desiredLines, applied, already, hunkDiagnostics, err := applyPatchHunks(ctx, nil, file.Hunks, !args.Strict)
	result.Diagnostics = append(result.Diagnostics, hunkDiagnostics...)
	result.AppliedHunks = applied
	result.AlreadyAppliedHunks = already

	if err != nil {
		if fallbackLines, ok := renderCreateFileContentFromHunks(file.Hunks); ok {
			result.Diagnostics = append(
				result.Diagnostics,
				warningDiagnostic(
					"create_fallback_from_hunk_body",
					"create-file patch did not apply cleanly; using hunk body as file content",
				),
			)
			desiredLines = fallbackLines
			result.AppliedHunks = len(file.Hunks)
			result.AlreadyAppliedHunks = 0
			err = nil
		}
	}

	desiredHasFinalNewline := newFileHasFinalNewline(file, desiredLines)

	if err != nil {
		resetUnwrittenAppliedHunks(&result, applied)
		result.Status = ApplyUnifiedDiffStatusConflict
		result.Message = err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	if exists {
		if err := requireNoSymlinkExistingRegularFileResolved(p, target.ResolvedPath); err != nil {
			result.Status = ApplyUnifiedDiffStatusError
			result.Message = err.Error()
			return fileApplyPlan{Result: result, Action: filePlanActionNoop}
		}

		tf, err := readTextFileNoSymlink(p, target.ResolvedPath)
		if err != nil {
			result.Status = ApplyUnifiedDiffStatusError
			result.Message = err.Error()
			return fileApplyPlan{Result: result, Action: filePlanActionNoop}
		}

		if ioutil.StringSlicesEqual(tf.Lines, desiredLines) && tf.HasFinalNewline == desiredHasFinalNewline {
			if file.NewFilePerm != 0 && tf.Perm != file.NewFilePerm {
				result.OK = true
				result.Status = ApplyUnifiedDiffStatusApplicable
				result.Message = fmt.Sprintf(
					"Create-file content already exists; file mode can be updated from %#o to %#o.",
					tf.Perm,
					file.NewFilePerm,
				)
				result.AlreadyAppliedHunks = len(file.Hunks)
				result.AppliedHunks = 0
				result.Diagnostics = append(
					result.Diagnostics,
					infoDiagnostic(
						"create_content_already_present_mode_change",
						"create-file content already exists; updating mode from %#o to %#o",
						tf.Perm,
						file.NewFilePerm,
					),
				)
				return fileApplyPlan{
					Result:          result,
					Action:          filePlanActionChmodExisting,
					DisplayPath:     target.DisplayPath,
					ResolvedPath:    target.ResolvedPath,
					Perm:            file.NewFilePerm,
					VerifyContent:   true,
					ExpectedContent: tf.Render(),
				}
			}
			result.OK = true
			result.Status = ApplyUnifiedDiffStatusAlreadyApplied
			result.Message = "Create-file patch is already applied because the target file already has the desired content."
			result.AlreadyAppliedHunks = len(file.Hunks)
			result.AppliedHunks = 0
			return fileApplyPlan{Result: result, Action: filePlanActionNoop}
		}

		result.Status = ApplyUnifiedDiffStatusConflict
		result.Message = "create-file patch target already exists with different content: " + target.DisplayPath
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	content := renderNewTextFile(desiredLines, desiredHasFinalNewline)
	if err := validateUnifiedDiffOutputContent(content); err != nil {
		result.Status = ApplyUnifiedDiffStatusError
		result.Message = err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}

	perm := file.NewFilePerm
	if perm == 0 {
		perm = defaultUnifiedDiffNewFilePerm
	}
	if err := verifyCreateParentPlanResolved(p, target.ResolvedPath, hardUnifiedDiffNewParentCreations); err != nil {
		result.Status = ApplyUnifiedDiffStatusError
		result.Message = "new file parent directory cannot be prepared: " + err.Error()
		return fileApplyPlan{Result: result, Action: filePlanActionNoop}
	}
	appendRelativeCreateTargetDiagnostic(&result, p, target)

	result.OK = true
	result.Status = ApplyUnifiedDiffStatusApplicable
	result.Message = "New file can be created by this patch."

	return fileApplyPlan{
		Result:       result,
		Action:       filePlanActionCreate,
		DisplayPath:  target.DisplayPath,
		ResolvedPath: target.ResolvedPath,
		Content:      content,
		Perm:         perm,
	}
}

func appendRelativeCreateTargetDiagnostic(
	result *ApplyUnifiedDiffFileOut,
	p fspolicy.FSPolicy,
	target targetResolution,
) {
	if result == nil ||
		strings.TrimSpace(target.DisplayPath) == "" ||
		strings.TrimSpace(target.ResolvedPath) == "" ||
		filepath.IsAbs(filepath.FromSlash(target.DisplayPath)) {
		return
	}

	result.Diagnostics = append(
		result.Diagnostics,
		infoDiagnostic(
			"relative_create_target_resolved",
			"relative new-file target %q resolved against work base dir %q to %q",
			target.DisplayPath,
			p.WorkBaseDir(),
			target.ResolvedPath,
		),
	)
}

func verifyCreateParentPlanResolved(p fspolicy.FSPolicy, resolvedPath string, maxNewDirs int) error {
	if strings.TrimSpace(resolvedPath) == "" {
		return errors.New("create plan has no resolved path")
	}

	parent := filepath.Dir(resolvedPath)
	if strings.TrimSpace(parent) == "" || parent == "." {
		return errors.New("create plan has invalid parent directory")
	}

	existing := parent
	missing := 0

	for {
		st, err := os.Lstat(existing)
		if err == nil {
			if st.IsDir() || (st.Mode()&os.ModeSymlink) != 0 {
				if err := p.VerifyDirResolved(existing); err != nil {
					return err
				}
				return nil
			}
			return fmt.Errorf("path component is not a directory: %s", existing)
		}

		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat error: %w", err)
		}

		missing++
		if maxNewDirs > 0 && missing > maxNewDirs {
			return fmt.Errorf("too many parent directories to create (max %d)", maxNewDirs)
		}

		next := filepath.Dir(existing)
		if next == existing {
			return fmt.Errorf("no existing parent directory for %s", parent)
		}
		existing = next
	}
}

func resetUnwrittenAppliedHunks(file *ApplyUnifiedDiffFileOut, simulatedApplied int) {
	if file == nil {
		return
	}
	if simulatedApplied > 0 {
		file.Diagnostics = append(
			file.Diagnostics,
			warningDiagnostic(
				"matched_but_not_written",
				"%d hunk(s) matched in memory but were not written for this file",
				simulatedApplied,
			),
		)
	}
	file.AppliedHunks = 0
}

func resetUnverifiedAppliedHunks(file *ApplyUnifiedDiffFileOut, simulatedApplied int) {
	if file == nil {
		return
	}
	if simulatedApplied > 0 {
		file.Diagnostics = append(
			file.Diagnostics,
			warningDiagnostic(
				"matched_but_not_verified",
				"%d hunk(s) were executed but the final on-disk state could not be verified",
				simulatedApplied,
			),
		)
	}
	file.AppliedHunks = 0
}

func executeFilePlan(p fspolicy.FSPolicy, plan fileApplyPlan) error {
	switch plan.Action {
	case filePlanActionNoop:
		return nil

	case filePlanActionCreate:
		perm := plan.Perm
		if perm == 0 {
			perm = defaultUnifiedDiffNewFilePerm
		}
		if strings.TrimSpace(plan.ResolvedPath) == "" {
			return errors.New("create plan has no resolved path")
		}
		parent := filepath.Dir(plan.ResolvedPath)
		if _, err := p.EnsureDirResolved(parent, hardUnifiedDiffNewParentCreations); err != nil {
			return err
		}
		return ioutil.WriteFileAtomicBytesResolved(
			p,
			plan.ResolvedPath,
			[]byte(plan.Content),
			perm,
			false,
		)

	case filePlanActionWriteExisting:
		return ioutil.WriteFileAtomicBytesResolvedWithPreCommitCheck(
			p,
			plan.ResolvedPath,
			[]byte(plan.Content),
			plan.Perm,
			true,
			func() error {
				return verifyFilePlanCurrentContent(p, plan)
			},
		)
	case filePlanActionChmodExisting:
		if plan.Perm == 0 {
			return errors.New("chmod plan has no file mode")
		}
		if err := verifyFilePlanCurrentContent(p, plan); err != nil {
			return err
		}
		if err := requireNoSymlinkExistingRegularFileResolved(p, plan.ResolvedPath); err != nil {
			return err
		}
		return os.Chmod(plan.ResolvedPath, plan.Perm)
	case filePlanActionDelete:
		if err := verifyFilePlanCurrentContent(p, plan); err != nil {
			return err
		}
		if err := requireNoSymlinkExistingRegularFileResolved(p, plan.ResolvedPath); err != nil {
			return err
		}
		return os.Remove(plan.ResolvedPath)

	default:
		return fmt.Errorf("unknown file plan action: %s", plan.Action)
	}
}

func verifyExecutedFilePlan(p fspolicy.FSPolicy, plan fileApplyPlan) error {
	switch plan.Action {
	case filePlanActionNoop:
		return nil

	case filePlanActionCreate:
		return verifyPlanFileContentAndMode(p, plan.ResolvedPath, plan.Content, plan.Perm, "created file")

	case filePlanActionWriteExisting:
		return verifyPlanFileContentAndMode(p, plan.ResolvedPath, plan.Content, plan.Perm, "patched file")

	case filePlanActionChmodExisting:
		return verifyPlanFileContentAndMode(p, plan.ResolvedPath, plan.ExpectedContent, plan.Perm, "chmod target")

	case filePlanActionDelete:
		if strings.TrimSpace(plan.ResolvedPath) == "" {
			return errors.New("delete plan has no resolved path")
		}
		exists, err := resolvedPathExists(plan.ResolvedPath)
		if err != nil {
			return fmt.Errorf("deleted file could not be checked after apply: %w", err)
		}
		if exists {
			return fmt.Errorf("deleted file is still present on disk after apply: %s", plan.ResolvedPath)
		}
		return nil

	default:
		return fmt.Errorf("unknown file plan action: %s", plan.Action)
	}
}

func verifyPlanFileContentAndMode(
	p fspolicy.FSPolicy,
	resolvedPath string,
	expectedContent string,
	perm os.FileMode,
	label string,
) error {
	if strings.TrimSpace(resolvedPath) == "" {
		return fmt.Errorf("%s plan has no resolved path", label)
	}

	tf, err := readTextFileNoSymlink(p, resolvedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is not present on disk after apply: %s", label, resolvedPath)
		}
		return fmt.Errorf("%s could not be read back after apply: %w", label, err)
	}
	if tf.Render() != expectedContent {
		return fmt.Errorf("%s content verification failed after apply: %s", label, resolvedPath)
	}
	if perm != 0 && runtime.GOOS != toolutil.GOOSWindows && tf.Perm != perm {
		return fmt.Errorf("%s mode verification failed after apply: want %#o got %#o", label, perm, tf.Perm)
	}
	return nil
}

func patchedFileHasFinalNewline(
	originalHasFinalNewline bool,
	originalLineCount int,
	file parsedPatchFile,
	lines []string,
) bool {
	if len(lines) == 0 {
		return false
	}
	if file.NewNoFinalNewline != nil {
		return !*file.NewNoFinalNewline
	}
	if file.OldNoFinalNewline != nil {
		return true
	}
	if originalLineCount == 0 {
		return true
	}
	return originalHasFinalNewline
}

func verifyFilePlanCurrentContent(p fspolicy.FSPolicy, plan fileApplyPlan) error {
	if !plan.VerifyContent {
		return nil
	}

	tf, err := readTextFileNoSymlink(p, plan.ResolvedPath)
	if err != nil {
		return fmt.Errorf("target file changed or became unreadable after planning: %w", err)
	}
	if tf.Render() != plan.ExpectedContent {
		return errors.New("target file changed after planning; re-run applyUnifiedDiff")
	}
	return nil
}

func readTextFileNoSymlink(p fspolicy.FSPolicy, resolvedPath string) (*ioutil.TextFile, error) {
	if err := requireNoSymlinkExistingRegularFileResolved(p, resolvedPath); err != nil {
		return nil, err
	}
	return ioutil.ReadTextFileUTF8(p, resolvedPath, hardTextProcessingBytes)
}

func requireNoSymlinkExistingRegularFileResolved(p fspolicy.FSPolicy, resolvedPath string) error {
	st, err := os.Lstat(resolvedPath)
	if err != nil {
		return err
	}
	if (st.Mode() & os.ModeSymlink) != 0 {
		return fmt.Errorf("%w: refusing to operate on symlink file: %s", fspolicy.ErrSymlinkDisallowed, resolvedPath)
	}
	if st.IsDir() {
		return fmt.Errorf("expected file but got directory: %s", resolvedPath)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("expected regular file: %s", resolvedPath)
	}
	_, err = p.RequireExistingRegularFileResolved(resolvedPath)
	return err
}

func newFileHasFinalNewline(file parsedPatchFile, lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	if file.NewNoFinalNewline != nil {
		return !*file.NewNoFinalNewline
	}
	if file.OldNoFinalNewline != nil {
		return true
	}
	return true
}

func validateUnifiedDiffOutputContent(content string) error {
	if int64(len(content)) > hardTextProcessingBytes {
		return fmt.Errorf(
			"patched file would be too large: %d bytes; max %d",
			len(content),
			hardTextProcessingBytes,
		)
	}
	if !utf8.ValidString(content) {
		return errors.New("patched content is not valid UTF-8")
	}
	return nil
}

func resolvePatchTarget(
	p fspolicy.FSPolicy,
	file parsedPatchFile,
	args ApplyUnifiedDiffArgs,
	candidateInfos []candidatePathInfo,
	isOnlyPatchFile bool,
) (target targetResolution, candidates []string, diagnostics []ApplyUnifiedDiffDiagnostic, status ApplyUnifiedDiffStatus, err error) {
	patchPaths := patchPathCandidates(file)
	var hardResolveErr error

	allCandidates := candidatePathDisplayList(patchPaths, candidateInfos)

	if explicit, ok, explicitErr := findExplicitTarget(file, args.FileTargets); explicitErr != nil {
		return targetResolution{}, allCandidates, diagnostics, ApplyUnifiedDiffStatusNeedsInfo, explicitErr
	} else if ok {
		target, err := resolveTargetPath(p, explicit.TargetPath)
		if err != nil {
			return targetResolution{}, allCandidates, diagnostics, ApplyUnifiedDiffStatusError, err
		}
		return target, allCandidates, diagnostics, "", nil
	}
	if file.isDelete() {
		var missingTarget targetResolution
		hasMissingTarget := false

		for _, path := range patchPaths {
			resolvedTarget, resolveErr := resolveTargetPath(p, path)
			if resolveErr != nil {
				hardResolveErr = firstHardTargetResolutionError(hardResolveErr, resolveErr)

				diagnostics = append(
					diagnostics,
					warningDiagnostic(
						"diff_path_unresolved",
						"diff path %q could not be resolved: %v",
						path,
						resolveErr,
					),
				)
				continue
			}

			exists, statErr := resolvedPathExists(resolvedTarget.ResolvedPath)
			if statErr != nil {
				return targetResolution{}, allCandidates, diagnostics, ApplyUnifiedDiffStatusError, statErr
			}
			if exists {
				return resolvedTarget, allCandidates, diagnostics, "", nil
			}
			if !hasMissingTarget {
				missingTarget = resolvedTarget
				hasMissingTarget = true
			}
		}

		matches := matchCandidatePaths(patchPaths, candidateInfos, true)
		if target, candidates, diagnostics, status, err, handled := resolveCandidateMatches(
			p,
			matches,
			allCandidates,
			diagnostics,
		); handled {
			return target, candidates, diagnostics, status, err
		}

		if hasMissingTarget {
			return missingTarget, allCandidates, diagnostics, "", nil
		}
		if hardResolveErr != nil {
			return targetResolution{},
				allCandidates,
				diagnostics,
				ApplyUnifiedDiffStatusError,
				fmt.Errorf("delete target path from diff violates filesystem policy: %w", hardResolveErr)
		}
		if len(patchPaths) > 0 {
			return targetResolution{}, allCandidates, diagnostics, ApplyUnifiedDiffStatusNeedsInfo, errors.New(
				"delete target path from diff could not be resolved",
			)
		}
	}
	canCreate := file.isCreateLike() || file.canCreateWhenMissing()
	needsExisting := !canCreate

	if needsExisting {
		for _, path := range patchPaths {
			target, err := resolveTargetPath(p, path)
			if err != nil {
				hardResolveErr = firstHardTargetResolutionError(hardResolveErr, err)
				diagnostics = append(
					diagnostics,
					warningDiagnostic(
						"diff_path_unresolved",
						"diff path %q could not be resolved: %v",
						path, err,
					),
				)
				continue
			}

			exists, statErr := resolvedPathExists(target.ResolvedPath)
			if statErr != nil {
				return targetResolution{}, allCandidates, diagnostics, ApplyUnifiedDiffStatusError, statErr
			}

			if exists {
				return target, allCandidates, diagnostics, "", nil
			}
		}
	}

	matches := matchCandidatePaths(patchPaths, candidateInfos, needsExisting)
	if target, candidates, diagnostics, status, err, handled := resolveCandidateMatches(
		p,
		matches,
		allCandidates,
		diagnostics,
	); handled {
		return target, candidates, diagnostics, status, err
	}
	if canCreate {
		if target, candidates, diagnostics, status, err, handled := resolveCreateTargetFromCandidatePaths(
			p, patchPaths, candidateInfos, allCandidates, diagnostics,
		); handled {
			return target, candidates, diagnostics, status, err
		}
	}
	if canCreate && len(patchPaths) > 0 {
		for _, path := range patchPaths {
			target, err := resolveTargetPath(p, path)
			if err != nil {
				hardResolveErr = firstHardTargetResolutionError(hardResolveErr, err)

				diagnostics = append(
					diagnostics,
					warningDiagnostic(
						"diff_path_unresolved",
						"diff path %q could not be resolved: %v",
						path, err,
					),
				)
				continue
			}
			return target, allCandidates, diagnostics, "", nil
		}
		if len(candidateInfos) > 0 && allPatchPathCandidatesAreRelative(patchPaths) {
			diagnostics = append(
				diagnostics,
				warningDiagnostic(
					"relative_create_target_not_inferred",
					"new-file target path from relative diff could not be inferred from candidatePaths; provide fileTargets[].targetPath or include a unique absolute base directory candidate",
				),
			)
			return targetResolution{},
				allCandidates,
				diagnostics,
				ApplyUnifiedDiffStatusNeedsInfo,
				errors.New("new-file target path from relative diff could not be inferred from candidatePaths")
		}
	}
	if hardResolveErr != nil {
		return targetResolution{},
			allCandidates,
			diagnostics,
			ApplyUnifiedDiffStatusError,
			fmt.Errorf("target path from diff violates filesystem policy: %w", hardResolveErr)
	}
	if isOnlyPatchFile && len(candidateInfos) == 1 {
		target, err := resolveTargetPath(p, candidateInfos[0].Path)
		if err != nil {
			return targetResolution{}, allCandidates, diagnostics, ApplyUnifiedDiffStatusError, err
		}
		return target, allCandidates, diagnostics, "", nil
	}

	if len(patchPaths) > 0 {
		msg := "target file path from diff does not exist locally"
		if len(candidateInfos) > 0 {
			msg += "; choose one of the candidate paths or provide fileTargets[].targetPath"
		}
		return targetResolution{}, allCandidates, diagnostics, ApplyUnifiedDiffStatusNeedsInfo, errors.New(msg)
	}

	return targetResolution{}, allCandidates, diagnostics, ApplyUnifiedDiffStatusNeedsInfo, errors.New(
		"diff does not include a usable target path",
	)
}

func firstHardTargetResolutionError(current, err error) error {
	if current != nil {
		return current
	}
	if isHardTargetResolutionError(err) {
		return err
	}
	return nil
}

func isHardTargetResolutionError(err error) bool {
	return errors.Is(err, fspolicy.ErrOutsideAllowedRoots) ||
		errors.Is(err, fspolicy.ErrInvalidPath)
}

func resolveCandidateMatches(
	p fspolicy.FSPolicy,
	matches []string,
	allCandidates []string,
	diagnostics []ApplyUnifiedDiffDiagnostic,
) (
	target targetResolution,
	candidates []string,
	outDiagnostics []ApplyUnifiedDiffDiagnostic,
	status ApplyUnifiedDiffStatus,
	err error,
	handled bool,
) {
	if len(matches) == 0 {
		return targetResolution{}, nil, diagnostics, "", nil, false
	}
	if len(matches) == 1 {
		target, err := resolveTargetPath(p, matches[0])
		if err != nil {
			return targetResolution{}, allCandidates, diagnostics, ApplyUnifiedDiffStatusError, err, true
		}
		return target, allCandidates, diagnostics, "", nil, true
	}

	diagnostics = append(
		diagnostics,
		warningDiagnostic("ambiguous_candidate_paths", "multiple candidatePaths match this file patch"),
	)
	return targetResolution{},
		limitStrings(uniqueStrings(matches), hardCandidatePathsPerFile),
		diagnostics,
		ApplyUnifiedDiffStatusNeedsInfo,
		errors.New("ambiguous target path"),
		true
}

func resolveCreateTargetFromCandidatePaths(
	p fspolicy.FSPolicy,
	patchPaths []string,
	candidateInfos []candidatePathInfo,
	allCandidates []string,
	diagnostics []ApplyUnifiedDiffDiagnostic,
) (
	target targetResolution,
	candidates []string,
	outDiagnostics []ApplyUnifiedDiffDiagnostic,
	status ApplyUnifiedDiffStatus,
	err error,
	handled bool,
) {
	inferences := inferCreateTargetsFromCandidatePaths(patchPaths, candidateInfos)
	if len(inferences) == 0 {
		return targetResolution{}, nil, diagnostics, "", nil, false
	}

	best := bestCreateTargetInferences(inferences)
	targetPaths := createInferenceTargetPaths(best)

	if len(targetPaths) == 1 {
		target, err := resolveTargetPath(p, targetPaths[0])
		if err != nil {
			return targetResolution{}, allCandidates, diagnostics, ApplyUnifiedDiffStatusError, err, true
		}

		source := best[0].SourcePath
		if source == "" {
			source = "candidatePaths"
		}
		diagnostics = append(
			diagnostics,
			infoDiagnostic(
				"create_target_inferred_from_candidate_paths",
				"new-file target inferred as %q from candidate path %q",
				target.DisplayPath,
				source,
			),
		)
		return target, allCandidates, diagnostics, "", nil, true
	}

	diagnostics = append(
		diagnostics,
		warningDiagnostic(
			"create_target_ambiguous_from_candidate_paths",
			"new-file target path is ambiguous after candidatePaths inference; provide fileTargets[].targetPath",
		),
	)
	return targetResolution{},
		limitStrings(targetPaths, hardCandidatePathsPerFile),
		diagnostics,
		ApplyUnifiedDiffStatusNeedsInfo,
		errors.New("new-file target path is ambiguous after candidatePaths inference"),
		true
}

func inferCreateTargetsFromCandidatePaths(
	patchPaths []string,
	candidateInfos []candidatePathInfo,
) []createTargetInference {
	out := make([]createTargetInference, 0)

	for _, patchPath := range patchPaths {
		patchNorm := normalizePathForCompare(patchPath)
		if patchNorm == "" || isDevNull(patchNorm) || isAbsoluteDiffPath(patchPath) {
			continue
		}

		patchDir := path.Dir(patchNorm)
		if patchDir == "." {
			patchDir = ""
		}
		patchDirParts := comparablePathParts(patchDir)

		for _, info := range candidateInfos {
			source := strings.TrimSpace(info.ResolvedPath)
			if source == "" {
				continue
			}

			sourceNorm := normalizePathForCompare(source)
			if sourceNorm == "" {
				continue
			}

			sourceIsDir := info.IsDir || candidatePathLooksLikeDirectory(info.Path)
			candidateDir := sourceNorm
			if !sourceIsDir {
				candidateDir = path.Dir(sourceNorm)
				if candidateDir == "." {
					candidateDir = ""
				}
			}

			if candidateDir != "" && len(patchDirParts) > 0 {
				candidateDirParts := comparablePathParts(candidateDir)
				maxPrefix := min(len(patchDirParts), len(candidateDirParts))

				for k := maxPrefix; k >= 1; k-- {
					prefix := strings.Join(patchDirParts[:k], "/")
					root, ok := trimComparableDirSuffix(candidateDir, prefix)
					if !ok {
						continue
					}

					out = append(out, createTargetInference{
						TargetPath:    filepath.FromSlash(joinComparableRootAndRel(root, patchNorm)),
						SourcePath:    info.Path,
						MatchedPrefix: prefix,
						Score:         k,
					})
					break
				}
			}

			if sourceIsDir {
				out = append(out, createTargetInference{
					TargetPath: filepath.FromSlash(joinComparableRootAndRel(sourceNorm, patchNorm)),
					SourcePath: info.Path,
					Score:      0,
				})
			}
		}
	}

	return sortAndDedupeCreateTargetInferences(out)
}

func sortAndDedupeCreateTargetInferences(in []createTargetInference) []createTargetInference {
	byTarget := map[string]createTargetInference{}

	for _, candidate := range in {
		key := normalizePathForCompare(candidate.TargetPath)
		if key == "" {
			continue
		}
		existing, ok := byTarget[key]
		if !ok || candidate.Score > existing.Score {
			byTarget[key] = candidate
		}
	}

	out := make([]createTargetInference, 0, len(byTarget))
	for _, candidate := range byTarget {
		out = append(out, candidate)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return normalizePathForCompare(out[i].TargetPath) < normalizePathForCompare(out[j].TargetPath)
	})

	return out
}

func bestCreateTargetInferences(in []createTargetInference) []createTargetInference {
	if len(in) == 0 {
		return nil
	}
	bestScore := in[0].Score
	var out []createTargetInference
	for _, candidate := range in {
		if candidate.Score != bestScore {
			break
		}
		out = append(out, candidate)
	}
	return out
}

func createInferenceTargetPaths(in []createTargetInference) []string {
	out := make([]string, 0, len(in))
	for _, candidate := range in {
		out = append(out, candidate.TargetPath)
	}
	return uniqueStrings(out)
}

func resolveTargetPath(p fspolicy.FSPolicy, displayPath string) (targetResolution, error) {
	displayPath = strings.TrimSpace(displayPath)
	if displayPath == "" {
		return targetResolution{}, errors.New("target path is empty")
	}

	resolved, err := p.ResolvePath(displayPath, "")
	if err != nil {
		return targetResolution{}, err
	}

	return targetResolution{
		DisplayPath:  displayPath,
		ResolvedPath: resolved,
	}, nil
}

func findExplicitTarget(
	file parsedPatchFile,
	targets []ApplyUnifiedDiffFileTarget,
) (ApplyUnifiedDiffFileTarget, bool, error) {
	fileKeyMatches := make([]ApplyUnifiedDiffFileTarget, 0, 1)
	pathMatches := make([]ApplyUnifiedDiffFileTarget, 0, 1)

	for _, target := range targets {
		if strings.TrimSpace(target.TargetPath) == "" {
			continue
		}

		fileKey := strings.TrimSpace(target.FileKey)
		if fileKey != "" {
			if file.fileKeyMatches(fileKey) {
				fileKeyMatches = append(fileKeyMatches, target)
			}
			continue
		}

		if explicitTargetPathMatchesFile(file, target) {
			pathMatches = append(pathMatches, target)
		}
	}

	if len(fileKeyMatches) > 1 {
		return ApplyUnifiedDiffFileTarget{}, false, fmt.Errorf(
			"multiple fileTargets match %s by fileKey; keep only one mapping for fileKey %q",
			file.FileKey,
			file.FileKey,
		)
	}
	if len(fileKeyMatches) == 1 {
		return fileKeyMatches[0], true, nil
	}

	if len(pathMatches) == 0 {
		return ApplyUnifiedDiffFileTarget{}, false, nil
	}
	if len(pathMatches) > 1 {
		return ApplyUnifiedDiffFileTarget{}, false, fmt.Errorf(
			"multiple fileTargets match %s by path; use fileKey %q to disambiguate",
			file.FileKey,
			file.FileKey,
		)
	}

	return pathMatches[0], true, nil
}

func explicitTargetPathMatchesFile(file parsedPatchFile, target ApplyUnifiedDiffFileTarget) bool {
	oldPath := strings.TrimSpace(target.OldPath)
	newPath := strings.TrimSpace(target.NewPath)

	if oldPath != "" && newPath != "" &&
		patchPathsEqual(oldPath, file.OldPath) &&
		patchPathsEqual(newPath, file.NewPath) {
		return true
	}
	if newPath != "" && patchPathsEqual(newPath, file.NewPath) {
		return true
	}
	if oldPath != "" && patchPathsEqual(oldPath, file.OldPath) {
		return true
	}
	return false
}

func patchPathsEqual(a, b string) bool {
	return normalizePathForCompare(a) == normalizePathForCompare(b)
}

func patchPathCandidates(file parsedPatchFile) []string {
	var out []string

	if file.isDelete() {
		if file.OldPath != "" && !isDevNull(file.OldPath) {
			out = append(out, file.OldPath)
		}
		return uniqueStrings(out)
	}

	if file.IsRename {
		if file.OldPath != "" && !isDevNull(file.OldPath) {
			out = append(out, file.OldPath)
		}
		if file.NewPath != "" && !isDevNull(file.NewPath) {
			out = append(out, file.NewPath)
		}

		return uniqueStrings(out)
	}

	if file.NewPath != "" && !isDevNull(file.NewPath) {
		out = append(out, file.NewPath)
	}
	if file.OldPath != "" && !isDevNull(file.OldPath) {
		out = append(out, file.OldPath)
	}

	return uniqueStrings(out)
}

func buildCandidatePathInfos(p fspolicy.FSPolicy, candidatePaths []string) []candidatePathInfo {
	out := make([]candidatePathInfo, 0, len(candidatePaths))
	seen := map[string]bool{}

	for _, path := range candidatePaths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}

		key := normalizePathForCompare(path)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true

		info := candidatePathInfo{
			Path:     path,
			NormPath: normalizePathForCompare(path),
			BasePath: basenameForCompare(path),
		}

		if resolved, err := p.ResolvePath(path, ""); err == nil {
			info.ResolvedPath = resolved
			info.NormResolved = normalizePathForCompare(resolved)
			info.BaseResolved = basenameForCompare(resolved)

			if st, statErr := os.Lstat(resolved); statErr == nil {
				info.Exists = true
				info.IsDir = st.IsDir()
			}
		}

		out = append(out, info)
	}

	return out
}

func resolvedPathExists(resolvedPath string) (bool, error) {
	_, err := os.Lstat(resolvedPath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func candidatePathDisplayList(patchPaths []string, infos []candidatePathInfo) []string {
	out := make([]string, 0, len(patchPaths)+len(infos))
	out = append(out, patchPaths...)
	for _, info := range infos {
		out = append(out, info.Path)
	}
	return limitStrings(uniqueStrings(out), hardCandidatePathsPerFile)
}

func matchCandidatePaths(patchPaths []string, infos []candidatePathInfo, requireExists bool) []string {
	if len(patchPaths) == 0 || len(infos) == 0 {
		return nil
	}

	exact := map[string]bool{}
	suffix := map[string]bool{}
	base := map[string]bool{}

	for _, patchPath := range patchPaths {
		np := normalizePathForCompare(patchPath)
		bp := basenameForCompare(patchPath)
		if np == "" {
			continue
		}

		for _, info := range infos {
			if info.IsDir {
				continue
			}
			if requireExists && !info.Exists {
				continue
			}

			if info.NormPath == np || info.NormResolved == np {
				exact[info.Path] = true
				continue
			}

			if strings.HasSuffix(info.NormPath, "/"+np) || strings.HasSuffix(info.NormResolved, "/"+np) {
				suffix[info.Path] = true
				continue
			}

			if bp != "" && (info.BasePath == bp || info.BaseResolved == bp) {
				base[info.Path] = true
			}
		}
	}

	if len(exact) > 0 {
		return mapKeys(exact)
	}
	if len(suffix) > 0 {
		return mapKeys(suffix)
	}
	if len(base) > 0 {
		return mapKeys(base)
	}

	return nil
}

func applyPatchHunks(
	ctx context.Context,
	lines []string,
	hunks []parsedHunk,
	fuzzy bool,
) (desiredLines []string, applied, already int, hunkDiagnostics []ApplyUnifiedDiffDiagnostic, err error) {
	next := toolutil.CloneStringSlice(lines)
	lineDelta := 0
	lineDrift := 0
	diagnostics := []ApplyUnifiedDiffDiagnostic{}

	for _, hunk := range hunks {
		if err := ctx.Err(); err != nil {
			return next, applied, already, diagnostics, err
		}

		res, err := applyHunkToLines(next, hunk, lineDelta, lineDrift, fuzzy)
		diagnostics = append(diagnostics, res.Diagnostics...)

		if err != nil {
			return next, applied, already, diagnostics, fmt.Errorf("failed at hunk %s: %w", hunk.Header, err)
		}

		if res.AlreadyApplied {
			already++
		} else {
			next = res.Lines
			applied++
		}

		if res.Matched {
			switch res.MatchBasis {
			case hunkMatchBasisOld:
				lineDrift = res.MatchStart - hunkOldHint(hunk, lineDelta)
			case hunkMatchBasisNew:
				lineDrift = res.MatchStart - hunkNewHint(hunk, 0)

			default:
				// Do not learn global drift from context-free insertion hunks.
			}
		}

		lineDelta += res.NewLen - res.OldLen
	}

	return next, applied, already, diagnostics, nil
}

func applyHunkToLines(
	lines []string,
	h parsedHunk,
	lineDelta int,
	lineDrift int,
	fuzzy bool,
) (hunkApplyResult, error) {
	oldBlock, oldKinds, newBlock := hunkBlocks(h)

	oldHint := hunkOldHint(h, lineDelta+lineDrift)
	insertHint := hunkInsertHint(h, lineDelta+lineDrift)
	newHint := hunkNewHint(h, lineDrift)

	if len(oldBlock) == 0 {
		if len(newBlock) == 0 {
			return hunkApplyResult{
				Lines:          lines,
				OldLen:         0,
				NewLen:         0,
				AlreadyApplied: true,
				Diagnostics: []ApplyUnifiedDiffDiagnostic{
					infoDiagnostic("empty_hunk", "empty hunk treated as already applied"),
				},
			}, nil
		}

		if already, ok := findAlreadyAppliedMatch(lines, newBlock, newHint, insertHint, fuzzy, fuzzy); ok {
			return alreadyAppliedHunkResult(lines, 0, len(newBlock), already, nil), nil
		}

		if fuzzy && len(lines) != 0 {
			return hunkApplyResult{
				OldLen: 0,
				NewLen: len(newBlock),
				Diagnostics: []ApplyUnifiedDiffDiagnostic{
					errorDiagnostic(
						"context_free_insert_unsafe",
						"context-free insert-only hunk has no old/context lines; refusing to use line-number hint in fuzzy mode",
					),
				},
			}, errors.New("insert-only hunk has no old/context lines; cannot safely place it without matching context")
		}

		insertAt := insertHint
		if insertAt < 0 || insertAt > len(lines) {
			if len(lines) != 0 {
				return hunkApplyResult{
					OldLen: 0,
					NewLen: len(newBlock),
					Diagnostics: []ApplyUnifiedDiffDiagnostic{
						errorDiagnostic(
							"insert_hint_out_of_range",
							"insert-only hunk line hint %d is outside file range 1..%d and the hunk has no old/context lines",
							insertHint+1,
							len(lines)+1,
						),
					},
				}, fmt.Errorf("insert-only hunk line hint %d is outside file range 1..%d and the hunk has no old/context lines", insertHint+1, len(lines)+1)
			}
			insertAt = clampInt(insertAt, 0, len(lines))
		}
		next := ioutil.ReplaceStringRange(lines, insertAt, insertAt, newBlock)

		return hunkApplyResult{
			Lines:  next,
			OldLen: 0,
			NewLen: len(newBlock),
			Diagnostics: []ApplyUnifiedDiffDiagnostic{
				infoDiagnostic("inserted_hunk", "inserted hunk at line %d", insertAt+1),
			},
			Matched:    true,
			MatchStart: insertAt,
			MatchBasis: hunkMatchBasisInsert,
		}, nil
	}

	blocksDiffer := !ioutil.StringSlicesEqual(oldBlock, newBlock)

	match, ok, diagnostics := findOldBlockPrimaryMatch(lines, oldBlock, oldHint, fuzzy)
	if ok {
		return applyMatchedOldBlock(lines, h, oldBlock, match, diagnostics), nil
	}

	// Handle single contiguous edit groups before generic fuzzy matching so we
	// preserve prefix/suffix-specific diagnostics and avoid weaker old-block
	// fuzzy matches stealing the hunk first.
	if fuzzy && !ioutil.StringSlicesEqual(oldBlock, newBlock) {
		if res, handled, err := tryApplySingleEditGroupHunk(lines, h); handled {
			return res, err
		}
	}

	if blocksDiffer {
		if already, ok := findAlreadyAppliedMatch(lines, newBlock, newHint, oldHint, fuzzy, fuzzy); ok {
			return alreadyAppliedHunkResult(lines, len(oldBlock), len(newBlock), already, diagnostics), nil
		}

		if fuzzy {
			if res, handled, err := tryApplySingleEditGroupHunk(lines, h); handled {
				res.Diagnostics = append(append([]ApplyUnifiedDiffDiagnostic{}, diagnostics...), res.Diagnostics...)
				return res, err
			}

			if match, ok, d := findOldBlockAdvancedFuzzyMatch(lines, oldBlock, oldKinds, oldHint); ok {
				diagnostics = append(diagnostics, d...)
				return applyMatchedOldBlock(lines, h, oldBlock, match, diagnostics), nil
			} else {
				diagnostics = append(diagnostics, d...)
			}
		}
	}

	if !blocksDiffer {
		if already, ok := findAlreadyAppliedMatch(lines, newBlock, newHint, oldHint, fuzzy, fuzzy); ok {
			return alreadyAppliedHunkResult(lines, len(oldBlock), len(newBlock), already, diagnostics), nil
		}
	}

	diagnostics = append(
		diagnostics,
		errorDiagnostic("hunk_match_failed", "old hunk block was not found and new hunk block was not already present"),
	)
	hRes := hunkApplyResult{
		Diagnostics: diagnostics,
	}
	return hRes, errors.New(
		"old hunk block was not found and new hunk block was not already present",
	)
}

func applyMatchedOldBlock(
	lines []string,
	h parsedHunk,
	oldBlock []string,
	match blockMatch,
	priorDiagnostics []ApplyUnifiedDiffDiagnostic,
) hunkApplyResult {
	replacement := buildHunkReplacementFromMatchedOld(
		lines[match.Start:match.Start+len(oldBlock)],
		h,
	)
	next := ioutil.ReplaceStringRange(lines, match.Start, match.Start+len(oldBlock), replacement)

	diagnostics := append([]ApplyUnifiedDiffDiagnostic{}, priorDiagnostics...)
	diagnostics = append(diagnostics, infoDiagnostic(
		"hunk_matched",
		"matched hunk at line %d using %s",
		match.Start+1,
		match.Method,
	))

	return hunkApplyResult{
		Lines:  next,
		OldLen: len(oldBlock),
		NewLen: len(replacement),

		Diagnostics: diagnostics,
		Matched:     true,
		MatchStart:  match.Start,
		MatchBasis:  hunkMatchBasisOld,
	}
}

func alreadyAppliedHunkResult(
	lines []string,
	oldLen int,
	newLen int,
	match blockMatch,
	priorDiagnostics []ApplyUnifiedDiffDiagnostic,
) hunkApplyResult {
	diagnostics := append([]ApplyUnifiedDiffDiagnostic{}, priorDiagnostics...)
	diagnostics = append(diagnostics, infoDiagnostic(
		"hunk_already_applied",
		"hunk already applied at line %d using %s",
		match.Start+1,
		match.Method,
	))

	return hunkApplyResult{
		Lines:          lines,
		OldLen:         oldLen,
		NewLen:         newLen,
		AlreadyApplied: true,
		Diagnostics:    diagnostics,
		Matched:        true,
		MatchStart:     match.Start,
		MatchBasis:     hunkMatchBasisNew,
	}
}

func tryApplySingleEditGroupHunk(
	lines []string,
	h parsedHunk,
) (result hunkApplyResult, handled bool, err error) {
	hunkSpec, ok := splitSingleEditGroupHunk(h)
	if !ok {
		return hunkApplyResult{}, false, nil
	}

	match, ok, diagnostics := findSingleEditGroupMatch(lines, hunkSpec)
	if !ok {
		if len(diagnostics) == 0 {
			return hunkApplyResult{}, false, nil
		}
		diagnostics = append(
			diagnostics,
			errorDiagnostic("single_edit_match_failed", "single-edit hunk could not be applied safely"),
		)
		hRes := hunkApplyResult{
			Diagnostics: diagnostics,
		}
		return hRes, true, errors.New(
			"single-edit hunk could not be applied safely",
		)
	}

	diagnostics = append([]ApplyUnifiedDiffDiagnostic{}, diagnostics...)

	if match.AlreadyApplied {
		diagnostics = append(diagnostics, infoDiagnostic(
			"hunk_already_applied",
			"hunk already applied at line %d using %s",
			match.EditStart+1,
			match.Method,
		))

		return hunkApplyResult{
			Lines:          lines,
			OldLen:         len(hunkSpec.OldEdit),
			NewLen:         len(hunkSpec.NewEdit),
			AlreadyApplied: true,
			Diagnostics:    diagnostics,
			Matched:        true,
			MatchStart:     match.EstimatedStart,
			MatchBasis:     hunkMatchBasisNone,
		}, true, nil
	}

	if match.EditStart < 0 || match.EditStart+len(hunkSpec.OldEdit) > len(lines) {
		return hunkApplyResult{Diagnostics: []ApplyUnifiedDiffDiagnostic{
			errorDiagnostic(
				"single_edit_invalid_location",
				"single-edit fallback selected invalid line %d",
				match.EditStart+1,
			),
		}}, true, errors.New("single-edit fallback selected invalid line")
	}

	next := ioutil.ReplaceStringRange(
		lines,
		match.EditStart,
		match.EditStart+len(hunkSpec.OldEdit),
		hunkSpec.NewEdit,
	)
	diagnostics = append(diagnostics, infoDiagnostic(
		"hunk_matched",
		"matched hunk at line %d using %s",
		match.EditStart+1,
		match.Method,
	))

	return hunkApplyResult{
		Lines:       next,
		OldLen:      len(hunkSpec.OldEdit),
		NewLen:      len(hunkSpec.NewEdit),
		Diagnostics: diagnostics,
		Matched:     true,
		MatchStart:  match.EstimatedStart,
		MatchBasis:  hunkMatchBasisNone,
	}, true, nil
}

func splitSingleEditGroupHunk(h parsedHunk) (singleEditHunkSpec, bool) {
	var hunkSpec singleEditHunkSpec
	phase := 0
	sawEdit := false

	for _, line := range h.Lines {
		switch line.Kind {
		case '+':
			if phase == 2 {
				return singleEditHunkSpec{}, false
			}
			phase = 1
			sawEdit = true
			hunkSpec.NewEdit = append(hunkSpec.NewEdit, line.Text)

		case '-':
			if phase == 2 {
				return singleEditHunkSpec{}, false
			}
			phase = 1
			sawEdit = true
			hunkSpec.OldEdit = append(hunkSpec.OldEdit, line.Text)

		default:
			if phase == 0 {
				hunkSpec.Prefix = append(hunkSpec.Prefix, line.Text)
			} else {
				phase = 2
				hunkSpec.Suffix = append(hunkSpec.Suffix, line.Text)
			}
		}
	}

	if !sawEdit {
		return singleEditHunkSpec{}, false
	}

	// A context-free insertion into an existing non-empty file is intentionally
	// handled, and normally rejected in fuzzy mode, by the insert-only branch.
	if len(hunkSpec.OldEdit) == 0 && len(hunkSpec.Prefix) == 0 && len(hunkSpec.Suffix) == 0 {
		return singleEditHunkSpec{}, false
	}

	return hunkSpec, true
}

func findSingleEditGroupMatch(
	lines []string,
	hunkSpec singleEditHunkSpec,
) (match singleEditMatch, ok bool, diagnostics []ApplyUnifiedDiffDiagnostic) {
	prefixStrong := strongContextBlock(hunkSpec.Prefix)
	suffixStrong := strongContextBlock(hunkSpec.Suffix)
	oldEditStrong := strongContextBlock(hunkSpec.OldEdit)
	newEditStrong := strongContextBlock(hunkSpec.NewEdit)

	for _, mode := range []compareMode{compareExact, compareTrimmed} {
		modeName := compareModeName(mode)

		var prefixMatches []int
		if prefixStrong {
			prefixMatches = findAllBlockMatches(lines, hunkSpec.Prefix, mode, hardUnifiedDiffDiagnosticCandidates+1)
		}

		var suffixMatches []int
		if suffixStrong {
			suffixMatches = findAllBlockMatches(lines, hunkSpec.Suffix, mode, hardUnifiedDiffDiagnosticCandidates+1)
		}

		prefixMatched := prefixStrong && len(prefixMatches) > 0
		suffixMatched := suffixStrong && len(suffixMatches) > 0

		if prefixMatched && suffixMatched {
			candidates := twoSidedSingleEditMatches(lines, hunkSpec, prefixMatches, suffixMatches, mode, modeName)
			contextDiagnostics := singleEditContextAmbiguityDiagnostics(prefixMatches, suffixMatches, modeName)
			match, ok, ambiguous, diag := selectUniqueSingleEditMatch(candidates, "two-sided "+modeName+" context")
			if ok {
				outDiagnostics := append([]ApplyUnifiedDiffDiagnostic{}, diagnostics...)
				outDiagnostics = append(outDiagnostics, contextDiagnostics...)
				outDiagnostics = append(outDiagnostics, diag...)
				return match, true, outDiagnostics
			}
			diagnostics = append(diagnostics, contextDiagnostics...)
			diagnostics = append(diagnostics, diag...)

			if ambiguous {
				return singleEditMatch{}, false, diagnostics
			}

			prefixUnique := len(prefixMatches) == 1
			suffixUnique := len(suffixMatches) == 1

			if prefixUnique && suffixUnique && len(hunkSpec.OldEdit) == 0 && len(hunkSpec.NewEdit) > 0 {
				alreadyCandidates, d := oneSidedSingleEditMatches(
					lines,
					hunkSpec,
					prefixMatches,
					suffixMatches,
					mode,
					modeName,
					true,
				)
				candidateDiagnostics := append(append([]ApplyUnifiedDiffDiagnostic{}, diagnostics...), d...)
				if match, ok, ambiguous, diag := selectUniqueSingleEditMatch(
					alreadyCandidates,
					"one-sided "+modeName+" already-applied insertion context after incompatible two-sided context",
				); ok {
					candidateDiagnostics = append(candidateDiagnostics, diag...)
					candidateDiagnostics = append(candidateDiagnostics, warningDiagnostic(
						"single_edit_incompatible_context_already_applied",
						"insert-only hunk appears already applied next to one unique context side even though the unique prefix and suffix are not adjacent; preserving intervening local lines",
					))
					return match, true, candidateDiagnostics
				} else if ambiguous {
					return singleEditMatch{}, false, append(candidateDiagnostics, diag...)
				}
			}

			switch {
			case prefixUnique && suffixUnique:
				diagnostics = append(
					diagnostics,
					warningDiagnostic(
						"single_edit_incompatible_context",
						"single-edit fallback found unique prefix and suffix %s context, but they do not describe a compatible edit location",
						modeName,
					),
				)
				return singleEditMatch{}, false, diagnostics

			case !prefixUnique && !suffixUnique:
				diagnostics = append(
					diagnostics,
					warningDiagnostic(
						"single_edit_non_unique_context",
						"single-edit fallback found non-unique prefix and suffix %s context and no compatible two-sided edit location",
						modeName,
					),
				)
				return singleEditMatch{}, false, diagnostics

			default:
				diagnostics = append(
					diagnostics,
					infoDiagnostic(
						"single_edit_one_sided_fallback",
						"single-edit fallback found no compatible two-sided %s context; falling back to the unique one-sided context",
						modeName,
					),
				)
			}
		}

		contextMatched := prefixMatched || suffixMatched

		if len(hunkSpec.OldEdit) == 0 {
			alreadyCandidates, d := oneSidedSingleEditMatches(
				lines,
				hunkSpec,
				prefixMatches,
				suffixMatches,
				mode,
				modeName,
				true,
			)
			candidateDiagnostics := append(append([]ApplyUnifiedDiffDiagnostic{}, diagnostics...), d...)
			if match, ok, ambiguous, diag := selectUniqueSingleEditMatch(
				alreadyCandidates,
				"one-sided "+modeName+" already-applied insertion context",
			); ok {
				return match, true, append(candidateDiagnostics, diag...)
			} else if ambiguous {
				return singleEditMatch{}, false, append(candidateDiagnostics, diag...)
			}
			diagnostics = candidateDiagnostics

			applyCandidates, d := oneSidedSingleEditMatches(
				lines,
				hunkSpec,
				prefixMatches,
				suffixMatches,
				mode,
				modeName,
				false,
			)
			candidateDiagnostics = append(append([]ApplyUnifiedDiffDiagnostic{}, diagnostics...), d...)
			if match, ok, ambiguous, diag := selectUniqueSingleEditMatch(
				applyCandidates,
				"one-sided "+modeName+" insertion context",
			); ok {
				return match, true, append(candidateDiagnostics, diag...)
			} else if ambiguous {
				return singleEditMatch{}, false, append(candidateDiagnostics, diag...)
			}
			diagnostics = candidateDiagnostics

			continue
		}

		applyCandidates, d := oneSidedSingleEditMatches(
			lines,
			hunkSpec,
			prefixMatches,
			suffixMatches,
			mode,
			modeName,
			false,
		)
		candidateDiagnostics := append(append([]ApplyUnifiedDiffDiagnostic{}, diagnostics...), d...)
		if match, ok, ambiguous, diag := selectUniqueSingleEditMatch(
			applyCandidates,
			"one-sided "+modeName+" edit context",
		); ok {
			return match, true, append(candidateDiagnostics, diag...)
		} else if ambiguous {
			return singleEditMatch{}, false, append(candidateDiagnostics, diag...)
		}
		diagnostics = candidateDiagnostics

		alreadyCandidates, d := oneSidedSingleEditMatches(
			lines,
			hunkSpec,
			prefixMatches,
			suffixMatches,
			mode,
			modeName,
			true,
		)
		candidateDiagnostics = append(append([]ApplyUnifiedDiffDiagnostic{}, diagnostics...), d...)
		if match, ok, ambiguous, diag := selectUniqueSingleEditMatch(
			alreadyCandidates,
			"one-sided "+modeName+" already-applied edit context",
		); ok {
			return match, true, append(candidateDiagnostics, diag...)
		} else if ambiguous {
			return singleEditMatch{}, false, append(candidateDiagnostics, diag...)
		}
		diagnostics = candidateDiagnostics

		if !contextMatched && oldEditStrong {
			oldOnlyCandidates := blockOnlySingleEditMatches(
				lines,
				hunkSpec.OldEdit,
				len(hunkSpec.Prefix),
				mode,
				"single-edit-old-"+modeName,
				false,
			)
			if match, ok, ambiguous, diag := selectUniqueSingleEditMatch(
				oldOnlyCandidates,
				"old edit body "+modeName,
			); ok {
				return match, true, append(append([]ApplyUnifiedDiffDiagnostic{}, diagnostics...), diag...)
			} else if ambiguous {
				return singleEditMatch{}, false, append(diagnostics, diag...)
			}
		}

		if !contextMatched && newEditStrong {
			newOnlyCandidates := blockOnlySingleEditMatches(
				lines,
				hunkSpec.NewEdit,
				len(hunkSpec.Prefix),
				mode,
				"single-edit-new-"+modeName+"-already",
				true,
			)
			if match, ok, ambiguous, diag := selectUniqueSingleEditMatch(
				newOnlyCandidates,
				"new edit body "+modeName,
			); ok {
				return match, true, append(append([]ApplyUnifiedDiffDiagnostic{}, diagnostics...), diag...)
			} else if ambiguous {
				return singleEditMatch{}, false, append(diagnostics, diag...)
			}
		}
	}

	return singleEditMatch{}, false, diagnostics
}

func singleEditContextAmbiguityDiagnostics(
	prefixMatches []int,
	suffixMatches []int,
	modeName string,
) []ApplyUnifiedDiffDiagnostic {
	diagnostics := []ApplyUnifiedDiffDiagnostic{}

	if len(prefixMatches) > 1 {
		diagnostics = append(diagnostics, warningDiagnostic(
			"single_edit_prefix_ambiguous",
			"single-edit fallback prefix %s context matched %d locations; refusing one-sided prefix anchor",
			modeName,
			len(prefixMatches),
		))
	}
	if len(suffixMatches) > 1 {
		diagnostics = append(diagnostics, warningDiagnostic(
			"single_edit_suffix_ambiguous",
			"single-edit fallback suffix %s context matched %d locations; refusing one-sided suffix anchor",
			modeName,
			len(suffixMatches),
		))
	}
	return diagnostics
}

func twoSidedSingleEditMatches(
	lines []string,
	hunkSpec singleEditHunkSpec,
	prefixMatches []int,
	suffixMatches []int,
	mode compareMode,
	modeName string,
) []singleEditMatch {
	out := make([]singleEditMatch, 0, 1)

	for _, prefixStart := range prefixMatches {
		editStart := prefixStart + len(hunkSpec.Prefix)

		for _, suffixStart := range suffixMatches {
			if suffixStart == editStart+len(hunkSpec.OldEdit) &&
				matchBlockAt(lines, hunkSpec.OldEdit, editStart, mode) {
				out = append(out, singleEditMatch{
					EditStart:      editStart,
					EstimatedStart: prefixStart,
					Method:         "single-edit-two-sided-" + modeName,
				})
			}

			if suffixStart == editStart+len(hunkSpec.NewEdit) &&
				matchBlockAt(lines, hunkSpec.NewEdit, editStart, mode) {
				out = append(out, singleEditMatch{
					EditStart:      editStart,
					EstimatedStart: prefixStart,
					Method:         "single-edit-two-sided-" + modeName + "-already",
					AlreadyApplied: true,
				})
			}
		}
	}

	return out
}

func oneSidedSingleEditMatches(
	lines []string,
	hunkSpec singleEditHunkSpec,
	prefixMatches []int,
	suffixMatches []int,
	mode compareMode,
	modeName string,
	already bool,
) (match []singleEditMatch, diagnostics []ApplyUnifiedDiffDiagnostic) {
	body := hunkSpec.OldEdit
	action := "apply"
	if already {
		body = hunkSpec.NewEdit
		action = "already"
	}

	// A one-sided empty "already" body would mark a deletion as already applied
	// merely because one context side exists. That is too weak. Deletion-already
	// detection is allowed by the two-sided prefix+suffix-adjacent case.
	if already && len(body) == 0 {
		return nil, nil
	}

	out := make([]singleEditMatch, 0, 2)

	if len(prefixMatches) > 1 {
		diagnostics = append(
			diagnostics,
			warningDiagnostic(
				"single_edit_prefix_ambiguous",
				"single-edit fallback prefix %s context matched %d locations; refusing one-sided prefix anchor",
				modeName,
				len(prefixMatches),
			),
		)
	} else if len(prefixMatches) == 1 {
		prefixStart := prefixMatches[0]
		editStart := prefixStart + len(hunkSpec.Prefix)
		if matchBlockAt(lines, body, editStart, mode) {
			out = append(out, singleEditMatch{
				EditStart:      editStart,
				EstimatedStart: prefixStart,
				Method:         "single-edit-prefix-" + action + "-" + modeName,
				AlreadyApplied: already,
			})
		}
	}

	if len(suffixMatches) > 1 {
		diagnostics = append(
			diagnostics,
			warningDiagnostic(
				"single_edit_suffix_ambiguous",
				"single-edit fallback suffix %s context matched %d locations; refusing one-sided suffix anchor",
				modeName,
				len(suffixMatches),
			),
		)
	} else if len(suffixMatches) == 1 {
		editStart := suffixMatches[0] - len(body)
		if matchBlockAt(lines, body, editStart, mode) {
			out = append(out, singleEditMatch{
				EditStart:      editStart,
				EstimatedStart: estimateSingleEditStart(editStart, len(hunkSpec.Prefix)),
				Method:         "single-edit-suffix-" + action + "-" + modeName,
				AlreadyApplied: already,
			})
		}
	}

	return out, diagnostics
}

func blockOnlySingleEditMatches(
	lines []string,
	block []string,
	prefixLen int,
	mode compareMode,
	method string,
	already bool,
) []singleEditMatch {
	matches := findAllBlockMatches(lines, block, mode, hardUnifiedDiffDiagnosticCandidates+1)
	out := make([]singleEditMatch, 0, len(matches))

	for _, start := range matches {
		out = append(out, singleEditMatch{
			EditStart:      start,
			EstimatedStart: estimateSingleEditStart(start, prefixLen),
			Method:         method,
			AlreadyApplied: already,
		})
	}

	return out
}

func selectUniqueSingleEditMatch(
	candidates []singleEditMatch,
	description string,
) (match singleEditMatch, ok, ambiguous bool, diagnostics []ApplyUnifiedDiffDiagnostic) {
	candidates = uniqueSingleEditMatches(candidates)

	switch len(candidates) {
	case 0:
		return singleEditMatch{}, false, false, nil
	case 1:
		return candidates[0], true, false, nil
	default:
		return singleEditMatch{}, false, true, []ApplyUnifiedDiffDiagnostic{
			warningDiagnostic(
				"single_edit_ambiguous",
				"single-edit fallback %s matched %d locations; refusing to choose",
				description,
				len(candidates),
			),
		}
	}
}

func uniqueSingleEditMatches(candidates []singleEditMatch) []singleEditMatch {
	out := make([]singleEditMatch, 0, len(candidates))
	seen := map[string]int{}

	for _, candidate := range candidates {
		key := fmt.Sprintf("%d:%t", candidate.EditStart, candidate.AlreadyApplied)
		if idx, ok := seen[key]; ok {
			if candidate.Method != "" && !strings.Contains(out[idx].Method, candidate.Method) {
				if out[idx].Method == "" {
					out[idx].Method = candidate.Method
				} else {
					out[idx].Method += "+" + candidate.Method
				}
			}
			continue
		}

		seen[key] = len(out)
		out = append(out, candidate)
	}

	return out
}

func strongContextBlock(block []string) bool {
	nonBlank := 0
	chars := 0

	for _, line := range block {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		nonBlank++
		chars += len(s)
	}

	return nonBlank > 0 && chars >= 4
}

func compareModeName(mode compareMode) string {
	if mode == compareTrimmed {
		return "trimmed"
	}
	return "exact"
}

func estimateSingleEditStart(editStart, prefixLen int) int {
	return max(0, editStart-prefixLen)
}

func hunkInsertHint(h parsedHunk, delta int) int {
	if h.OldCount == 0 {
		return h.OldStart + delta
	}
	return hunkOldHint(h, delta)
}

func hunkOldHint(h parsedHunk, delta int) int {
	if h.OldStart <= 0 {
		return clampInt(delta, 0, 1<<30)
	}
	return h.OldStart - 1 + delta
}

func hunkNewHint(h parsedHunk, delta int) int {
	if h.NewStart <= 0 {
		return clampInt(delta, 0, 1<<30)
	}
	return h.NewStart - 1 + delta
}

func findOldBlockPrimaryMatch(
	lines, block []string,
	hint int,
	fuzzy bool,
) (match blockMatch, ok bool, diagnostics []ApplyUnifiedDiffDiagnostic) {
	if len(block) == 0 {
		return blockMatch{Start: clampInt(hint, 0, len(lines)), Method: "empty"}, true, diagnostics
	}
	if !fuzzy {
		if match, ok := findOldBlockLocalMatch(lines, block, hint, false); ok {
			return match, true, diagnostics
		}

		if m, ok, count := findUniqueGlobal(lines, block, compareExact); ok {
			return blockMatch{Start: m, Method: "exact-global"}, true, diagnostics
		} else if count > 1 {
			diagnostics = append(
				diagnostics,
				warningDiagnostic("exact_old_block_ambiguous", "exact old block matched %d locations", count),
			)
		}

		return blockMatch{}, false, diagnostics
	}

	if m, ok, count := findUniqueGlobal(lines, block, compareExact); ok {
		return blockMatch{Start: m, Method: "exact-global"}, true, diagnostics
	} else if count > 1 {
		if m, nearOK := findUniqueNear(
			lines,
			block,
			hint,
			hardUnifiedDiffHunkNearbyLineTolerance,
			compareExact,
		); nearOK {
			diagnostics = append(
				diagnostics,
				warningDiagnostic(
					"exact_old_block_ambiguous_hint_selected",
					"exact old block matched %d locations; selected the unique match near the hunk line hint at line %d",
					count,
					m+1,
				),
			)
			return blockMatch{Start: m, Method: "exact-near-hint-disambiguated"}, true, diagnostics
		}
		diagnostics = append(
			diagnostics,
			warningDiagnostic("exact_old_block_ambiguous",
				"exact old block matched %d locations; refusing to choose by line hint alone", count),
		)
	}

	if m, ok, count := findUniqueGlobal(lines, block, compareTrimmed); ok {
		return blockMatch{Start: m, Method: "trimmed-global"}, true, diagnostics
	} else if count > 1 {
		if m, nearOK := findUniqueNear(
			lines,
			block,
			hint,
			hardUnifiedDiffHunkNearbyLineTolerance,
			compareTrimmed,
		); nearOK {
			diagnostics = append(
				diagnostics,
				warningDiagnostic(
					"trimmed_old_block_ambiguous_hint_selected",
					"trimmed old block matched %d locations; selected the unique match near the hunk line hint at line %d",
					count,
					m+1,
				),
			)
			return blockMatch{Start: m, Method: "trimmed-near-hint-disambiguated"}, true, diagnostics
		}
		diagnostics = append(
			diagnostics,
			warningDiagnostic("trimmed_old_block_ambiguous",
				"trimmed old block matched %d locations; refusing to choose by distant line hint", count),
		)
	}
	return blockMatch{}, false, diagnostics
}

func findOldBlockAdvancedFuzzyMatch(
	lines, block []string,
	kinds []byte,
	hint int,
) (match blockMatch, ok bool, diagnostics []ApplyUnifiedDiffDiagnostic) {
	if m, ok, d := findDeletionAnchoredWindow(lines, block, kinds, hint); ok {
		diagnostics = append(diagnostics, d...)
		return m, true, diagnostics
	} else {
		diagnostics = append(diagnostics, d...)
	}

	if m, ok, d := findBestScoredWindow(lines, block, kinds, hint); ok {
		diagnostics = append(diagnostics, d...)
		return m, true, diagnostics
	} else {
		diagnostics = append(diagnostics, d...)
	}

	return blockMatch{}, false, diagnostics
}

func findOldBlockLocalMatch(
	lines, block []string,
	hint int,
	fuzzy bool,
) (blockMatch, bool) {
	if len(block) == 0 {
		return blockMatch{Start: clampInt(hint, 0, len(lines)), Method: "empty"}, true
	}

	if matchBlockAt(lines, block, hint, compareExact) {
		return blockMatch{Start: hint, Method: "exact-hint"}, true
	}

	if m, ok := findUniqueNear(lines, block, hint, hardUnifiedDiffHunkNearbyLineTolerance, compareExact); ok {
		return blockMatch{Start: m, Method: "exact-near"}, true
	}

	if !fuzzy {
		return blockMatch{}, false
	}

	if matchBlockAt(lines, block, hint, compareTrimmed) {
		return blockMatch{Start: hint, Method: "trimmed-hint"}, true
	}

	if m, ok := findUniqueNear(lines, block, hint, hardUnifiedDiffHunkNearbyLineTolerance, compareTrimmed); ok {
		return blockMatch{Start: m, Method: "trimmed-near"}, true
	}

	return blockMatch{}, false
}

func findDeletionAnchoredWindow(
	lines, block []string,
	kinds []byte,
	hint int,
) (match blockMatch, ok bool, diagnostics []ApplyUnifiedDiffDiagnostic) {
	if len(block) == 0 || len(block) > len(lines) {
		return blockMatch{}, false, nil
	}

	deletionPositions := make([]int, 0, 4)
	for i, kind := range kinds {
		if kind == '-' {
			deletionPositions = append(deletionPositions, i)
		}
	}
	if len(deletionPositions) == 0 {
		return blockMatch{}, false, nil
	}

	best := blockMatch{Start: -1, Score: -1}
	secondScore := -1.0
	candidates := 0

	for start := 0; start+len(block) <= len(lines); start++ {
		requiredOK := true
		for _, pos := range deletionPositions {
			if pos < 0 || pos >= len(block) {
				requiredOK = false
				break
			}
			if !lineEquals(lines[start+pos], block[pos], compareTrimmed) {
				requiredOK = false
				break
			}
		}
		if !requiredOK {
			continue
		}

		contextTotal := 0
		contextScore := 0
		for i := range block {
			if i < len(kinds) && kinds[i] == '-' {
				continue
			}

			contextTotal++
			switch {
			case lines[start+i] == block[i]:
				contextScore += 2
			case strings.TrimSpace(lines[start+i]) == strings.TrimSpace(block[i]):
				contextScore++
			}
		}

		contextRatio := 1.0
		if contextTotal > 0 {
			contextRatio = float64(contextScore) / float64(contextTotal*2)
		}

		// If the hunk has meaningful context, require at least half of it to
		// still match. If it has little/no context, uniqueness and deletion
		// anchors are the safety mechanism.
		if contextTotal >= 2 && contextRatio < 0.50 {
			continue
		}

		candidates++
		score := 0.90 + contextRatio*0.08 - fuzzyHintPenalty(start, hint)
		if best.Start < 0 || score > best.Score {
			secondScore = best.Score
			best = blockMatch{
				Start:  start,
				Method: "deletion-anchored-fuzzy",
				Score:  score,
			}
		} else if score > secondScore {
			secondScore = score
		}
	}

	if best.Start < 0 {
		return blockMatch{}, false, nil
	}

	if candidates > 1 && best.Score-secondScore < 0.05 {
		return blockMatch{}, false, []ApplyUnifiedDiffDiagnostic{
			warningDiagnostic("deletion_anchored_ambiguous", "deletion-anchored fuzzy search was ambiguous"),
		}
	}

	return best, true, []ApplyUnifiedDiffDiagnostic{
		infoDiagnostic(
			"deletion_anchored_selected",
			"deletion-anchored fuzzy search selected line %d with score %.3f",
			best.Start+1,
			best.Score,
		),
	}
}

func buildHunkReplacementFromMatchedOld(actualOld []string, h parsedHunk) []string {
	out := make([]string, 0, len(actualOld)+4)
	oldIdx := 0

	for _, line := range h.Lines {
		switch line.Kind {
		case '+':
			out = append(out, line.Text)

		case '-':
			if oldIdx < len(actualOld) {
				oldIdx++
			}

		default:
			if oldIdx < len(actualOld) {
				out = append(out, actualOld[oldIdx])
			} else {
				out = append(out, line.Text)
			}
			oldIdx++
		}
	}

	return out
}

func findAlreadyAppliedMatch(
	lines, newBlock []string,
	newHint, oldHint int,
	fuzzy bool,
	allowGlobal bool,
) (match blockMatch, ok bool) {
	if len(newBlock) == 0 {
		return blockMatch{}, false
	}

	if allowGlobal {
		if m, ok, _ := findUniqueGlobal(lines, newBlock, compareExact); ok {
			return blockMatch{Start: m, Method: "already-exact-global"}, true
		}
	} else {
		if matchBlockAt(lines, newBlock, newHint, compareExact) {
			return blockMatch{Start: newHint, Method: "already-exact-new-hint"}, true
		}
		if matchBlockAt(lines, newBlock, oldHint, compareExact) {
			return blockMatch{Start: oldHint, Method: "already-exact-old-hint"}, true
		}

		if m, ok := findUniqueNear(lines, newBlock, newHint, hardUnifiedDiffHunkNearbyLineTolerance, compareExact); ok {
			return blockMatch{Start: m, Method: "already-exact-near"}, true
		}
	}

	if fuzzy {
		if allowGlobal {
			if m, ok, _ := findUniqueGlobal(lines, newBlock, compareTrimmed); ok {
				return blockMatch{Start: m, Method: "already-trimmed-global"}, true
			}
		} else {
			if matchBlockAt(lines, newBlock, newHint, compareTrimmed) {
				return blockMatch{Start: newHint, Method: "already-trimmed-new-hint"}, true
			}
			if matchBlockAt(lines, newBlock, oldHint, compareTrimmed) {
				return blockMatch{Start: oldHint, Method: "already-trimmed-old-hint"}, true
			}

			if m, ok := findUniqueNear(
				lines,
				newBlock,
				newHint,
				hardUnifiedDiffHunkNearbyLineTolerance,
				compareTrimmed,
			); ok {
				return blockMatch{Start: m, Method: "already-trimmed-near"}, true
			}
		}
	}

	return blockMatch{}, false
}

func findUniqueNear(lines, block []string, hint, radius int, mode compareMode) (int, bool) {
	if len(block) == 0 {
		return clampInt(hint, 0, len(lines)), true
	}

	start := max(0, hint-radius)
	end := min(len(lines)-len(block), hint+radius)

	matches := []int{}
	for i := start; i <= end; i++ {
		if matchBlockAt(lines, block, i, mode) {
			matches = append(matches, i)
			if len(matches) > 1 {
				return 0, false
			}
		}
	}

	if len(matches) == 1 {
		return matches[0], true
	}

	return 0, false
}

func findUniqueGlobal(lines, block []string, mode compareMode) (start int, ok bool, count int) {
	if len(block) == 0 {
		return 0, false, 0
	}

	first := -1

	for i := 0; i+len(block) <= len(lines); i++ {
		if matchBlockAt(lines, block, i, mode) {
			count++
			if first < 0 {
				first = i
			}
			if count > hardUnifiedDiffDiagnosticCandidates {
				break
			}
		}
	}

	return first, count == 1, count
}

func findAllBlockMatches(lines, block []string, mode compareMode, limit int) []int {
	if len(block) == 0 || len(block) > len(lines) {
		return nil
	}

	matches := []int{}

	for i := 0; i+len(block) <= len(lines); i++ {
		if !matchBlockAt(lines, block, i, mode) {
			continue
		}

		matches = append(matches, i)
		if limit > 0 && len(matches) >= limit {
			break
		}
	}

	return matches
}

func matchBlockAt(lines, block []string, start int, mode compareMode) bool {
	if start < 0 || start+len(block) > len(lines) {
		return false
	}

	for i := range block {
		if !lineEquals(lines[start+i], block[i], mode) {
			return false
		}
	}

	return true
}

func findBestScoredWindow(
	lines, block []string,
	kinds []byte,
	hint int,
) (match blockMatch, ok bool, diagnostics []ApplyUnifiedDiffDiagnostic) {
	if len(block) == 0 || len(block) > len(lines) {
		return blockMatch{}, false, nil
	}

	best := blockMatch{Start: -1}
	secondScore := -1.0
	candidates := 0

	for i := 0; i+len(block) <= len(lines); i++ {
		score, requiredOK := scoreFuzzyWindow(lines[i:i+len(block)], block, kinds)
		if !requiredOK || score < 0.82 {
			continue
		}

		candidates++
		scoreWithHint := score - fuzzyHintPenalty(i, hint)

		if best.Start < 0 || scoreWithHint > best.Score {
			secondScore = best.Score
			best = blockMatch{
				Start:  i,
				Method: "scored-fuzzy",
				Score:  scoreWithHint,
			}
		} else if scoreWithHint > secondScore {
			secondScore = scoreWithHint
		}
	}

	if best.Start < 0 {
		return blockMatch{}, false, []ApplyUnifiedDiffDiagnostic{
			warningDiagnostic("scored_fuzzy_no_safe_candidate", "scored fuzzy search found no safe candidate"),
		}
	}

	if candidates > 1 && best.Score-secondScore < 0.08 {
		return blockMatch{}, false, []ApplyUnifiedDiffDiagnostic{
			warningDiagnostic("scored_fuzzy_ambiguous", "scored fuzzy search was ambiguous"),
		}
	}

	return best, true, []ApplyUnifiedDiffDiagnostic{
		infoDiagnostic(
			"scored_fuzzy_selected",
			"scored fuzzy search selected line %d with score %.3f",
			best.Start+1,
			best.Score,
		),
	}
}

func infoDiagnostic(code, format string, args ...any) ApplyUnifiedDiffDiagnostic {
	return newApplyUnifiedDiffDiagnostic(ApplyUnifiedDiffDiagnosticLevelInfo, code, format, args...)
}

func warningDiagnostic(code, format string, args ...any) ApplyUnifiedDiffDiagnostic {
	return newApplyUnifiedDiffDiagnostic(ApplyUnifiedDiffDiagnosticLevelWarning, code, format, args...)
}

func errorDiagnostic(code, format string, args ...any) ApplyUnifiedDiffDiagnostic {
	return newApplyUnifiedDiffDiagnostic(ApplyUnifiedDiffDiagnosticLevelError, code, format, args...)
}

func newApplyUnifiedDiffDiagnostic(
	level ApplyUnifiedDiffDiagnosticLevel,
	code string,
	format string,
	args ...any,
) ApplyUnifiedDiffDiagnostic {
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	return ApplyUnifiedDiffDiagnostic{
		Level:   level,
		Code:    code,
		Message: message,
	}
}

func cloneApplyUnifiedDiffDiagnostics(in []ApplyUnifiedDiffDiagnostic) []ApplyUnifiedDiffDiagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]ApplyUnifiedDiffDiagnostic, len(in))
	copy(out, in)
	return out
}

func fuzzyHintPenalty(start, hint int) float64 {
	return float64(min(absInt(start-hint), hardUnifiedDiffHunkNearbyLineTolerance)) * 0.001
}

func scoreFuzzyWindow(actual, expected []string, kinds []byte) (float64, bool) {
	if len(actual) != len(expected) {
		return 0, false
	}

	total := 0.0
	score := 0.0
	hasRequiredDeletion := false

	for i := range expected {
		weight := 1.0
		required := false

		if i < len(kinds) && kinds[i] == '-' {
			weight = 4.0
			required = true
			hasRequiredDeletion = true
		}

		total += weight

		if actual[i] == expected[i] {
			score += weight
			continue
		}

		if strings.TrimSpace(actual[i]) == strings.TrimSpace(expected[i]) {
			score += weight * 0.86
			continue
		}

		if required {
			return 0, false
		}
	}

	if total <= 0 {
		return 0, false
	}

	value := score / total

	if !hasRequiredDeletion && value < 0.92 {
		return value, false
	}

	return value, true
}

func renderNewTextFile(lines []string, hasFinalNewline bool) string {
	tf := &ioutil.TextFile{
		Newline:         ioutil.NewlineLF,
		HasFinalNewline: hasFinalNewline,
		Lines:           toolutil.CloneStringSlice(lines),
	}
	return tf.Render()
}

func renderCreateFileContentFromHunks(hunks []parsedHunk) ([]string, bool) {
	desired := make([]string, 0)

	for _, hunk := range hunks {
		_, _, newBlock := hunkBlocks(hunk)
		if len(newBlock) == 0 {
			return nil, false
		}
		desired = append(desired, newBlock...)
	}

	if len(desired) == 0 {
		return nil, false
	}

	return desired, true
}

func hunkBlocks(h parsedHunk) (oldBlock []string, oldKinds []byte, newBlock []string) {
	for _, line := range h.Lines {
		switch line.Kind {
		case '+':
			newBlock = append(newBlock, line.Text)
		case '-':
			oldBlock = append(oldBlock, line.Text)
			oldKinds = append(oldKinds, '-')
		default:
			oldBlock = append(oldBlock, line.Text)
			oldKinds = append(oldKinds, ' ')
			newBlock = append(newBlock, line.Text)
		}
	}

	return oldBlock, oldKinds, newBlock
}

func lineEquals(a, b string, mode compareMode) bool {
	if mode == compareTrimmed {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	return a == b
}

func basenameForCompare(inPath string) string {
	n := normalizePathForCompare(inPath)
	if n == "" {
		return ""
	}
	parts := strings.Split(n, "/")
	return parts[len(parts)-1]
}

func limitStrings(in []string, limit int) []string {
	if limit <= 0 || len(in) <= limit {
		return in
	}
	return in[:limit]
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return uniqueStrings(out)
}

func uniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}

	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := normalizePathForCompare(s)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}

	return out
}

func normalizePathForCompare(pathValue string) string {
	pathValue = strings.TrimSpace(pathValue)
	if pathValue == "" {
		return ""
	}

	pathValue = strings.ReplaceAll(pathValue, "\\", "/")
	preserveUNC := runtime.GOOS == toolutil.GOOSWindows && strings.HasPrefix(pathValue, "//")
	if preserveUNC {
		pathValue = "//" + collapseRepeatedSlashes(strings.TrimLeft(pathValue[2:], "/"))
	} else {
		pathValue = collapseRepeatedSlashes(pathValue)
	}

	pathValue = path.Clean(pathValue)
	if preserveUNC && strings.HasPrefix(pathValue, "/") && !strings.HasPrefix(pathValue, "//") {
		pathValue = "/" + pathValue
	}
	if pathValue == "." {
		pathValue = ""
	}

	if runtime.GOOS == toolutil.GOOSWindows {
		pathValue = strings.ToLower(pathValue)
	}

	return pathValue
}

func comparablePathParts(pathValue string) []string {
	pathValue = strings.Trim(pathValue, "/")
	if pathValue == "" {
		return nil
	}
	return strings.Split(pathValue, "/")
}

func trimComparableDirSuffix(candidateDir, suffix string) (string, bool) {
	candidateDir = strings.TrimRight(strings.TrimSpace(candidateDir), "/")
	suffix = strings.Trim(strings.TrimSpace(suffix), "/")
	if candidateDir == "" || suffix == "" {
		return "", false
	}
	if candidateDir == suffix {
		return "", true
	}
	marker := "/" + suffix
	if before, ok := strings.CutSuffix(candidateDir, marker); ok {
		return before, true
	}
	return "", false
}

func joinComparableRootAndRel(root, rel string) string {
	root = strings.TrimSpace(root)
	rel = strings.TrimLeft(strings.TrimSpace(rel), "/")
	if root == "" {
		return rel
	}
	if root == "/" {
		return "/" + rel
	}
	return strings.TrimRight(root, "/") + "/" + rel
}

func candidatePathLooksLikeDirectory(inPath string) bool {
	s := strings.TrimSpace(inPath)
	return strings.HasSuffix(s, "/") || strings.HasSuffix(s, `\`)
}

func collapseRepeatedSlashes(s string) string {
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func isMarkdownFenceLine(line string) bool {
	s := strings.TrimSpace(line)
	return strings.HasPrefix(s, "```") ||
		strings.HasPrefix(s, "~~~")
}

func (f parsedPatchFile) isModeOnly() bool {
	return len(f.Hunks) == 0 &&
		f.NewFilePerm != 0 &&
		!f.isCreate() &&
		!f.isDelete()
}

func (f parsedPatchFile) fileKeyMatches(fileKey string) bool {
	fileKey = strings.TrimSpace(fileKey)
	if fileKey == "" {
		return false
	}
	if fileKey == f.FileKey {
		return true
	}
	for _, alias := range f.FileKeyAliases {
		if fileKey == strings.TrimSpace(alias) {
			return true
		}
	}
	return false
}

func (f parsedPatchFile) canCreateWhenMissing() bool {
	if f.isCreate() {
		return f.NewPath != "" && !isDevNull(f.NewPath)
	}
	return !f.isDelete() && f.looksLikeCreateOnlyHunks()
}

func (f parsedPatchFile) isCreateLike() bool {
	if f.isCreate() {
		return f.NewPath != "" && !isDevNull(f.NewPath)
	}
	return f.NewFilePerm != 0 &&
		f.NewPath != "" &&
		!isDevNull(f.NewPath) &&
		f.looksLikeCreateOnlyHunks()
}

func (f parsedPatchFile) looksLikeCreateOnlyHunks() bool {
	return !f.hasDeletedLines() && f.allHunksHaveNoOldContent()
}

func (f parsedPatchFile) hasDeletedLines() bool {
	return f.DeletedLines > 0
}

func (f parsedPatchFile) allHunksHaveNoOldContent() bool {
	if len(f.Hunks) == 0 {
		return false
	}
	for _, h := range f.Hunks {
		if h.hasOldContent() {
			return false
		}
	}
	return true
}

func (f parsedPatchFile) isCreate() bool {
	return f.IsNewFile || isDevNull(f.OldPath)
}

func (f parsedPatchFile) isDelete() bool {
	return f.IsDeletedFile || isDevNull(f.NewPath)
}

func (a filePlanAction) mutates() bool {
	switch a {
	case filePlanActionCreate, filePlanActionWriteExisting, filePlanActionChmodExisting, filePlanActionDelete:
		return true
	default:
		return false
	}
}

func (s looseHunkState) declaredComplete() bool {
	return s.countsKnown() && s.oldSeen >= s.hunk.OldCount && s.newSeen >= s.hunk.NewCount
}

func (s looseHunkState) wouldOverflow(oldInc, newInc int) bool {
	return s.countsKnown() && (s.oldSeen+oldInc > s.hunk.OldCount || s.newSeen+newInc > s.hunk.NewCount)
}

func (s looseHunkState) countsKnown() bool {
	return s.hunk != nil && s.hunk.OldCount >= 0 && s.hunk.NewCount >= 0
}

func (h parsedHunk) bodyCounts() (oldCount, newCount int) {
	for _, line := range h.Lines {
		switch line.Kind {
		case '+':
			newCount++
		case '-':
			oldCount++
		default:
			oldCount++
			newCount++
		}
	}
	return oldCount, newCount
}

func (h parsedHunk) hasOldContent() bool {
	for _, line := range h.Lines {
		if line.Kind != '+' {
			return true
		}
	}
	return false
}

func allPatchPathCandidatesAreRelative(paths []string) bool {
	found := false
	for _, candidate := range paths {
		if strings.TrimSpace(candidate) == "" || isDevNull(candidate) {
			continue
		}
		found = true
		if isAbsoluteDiffPath(candidate) {
			return false
		}
	}
	return found
}

func isAbsoluteDiffPath(inPath string) bool {
	inPath = strings.TrimSpace(inPath)
	if inPath == "" || isDevNull(inPath) {
		return false
	}
	return filepath.IsAbs(filepath.FromSlash(inPath))
}

func isNoNewlineAtEOFMarker(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), `\ No newline at end of file`)
}

func isDevNull(inPath string) bool {
	return strings.TrimSpace(inPath) == pathDevNull
}
