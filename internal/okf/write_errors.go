package okf

import "errors"

// Authoring sentinels. Every refusal is Permanent for the error digester; only
// ErrConflict is Transient (the model reads the concept again and retries).
var (
	ErrReservedField        = errors.New("generated and verified are reserved: the tool stamps authorship itself and human verification is recorded only through the /verify session command")
	ErrUnmergeable          = errors.New("the concept's frontmatter cannot be merged safely")
	ErrSymlinkTarget        = errors.New("write target is a symbolic link")
	ErrAlreadyExists        = errors.New("a file already exists at the path")
	ErrInvalidCreationPath  = errors.New("invalid concept path")
	ErrReservedName         = errors.New("index.md and log.md are maintained by the tool and cannot be concepts")
	ErrUnresolvedSuperseded = errors.New("superseded_by does not resolve to a concept of the bundle")
	ErrGateRefused          = errors.New("the write would introduce a conformance error")
	ErrOversize             = errors.New("the resulting file exceeds the per-file cap")
	ErrTypeProtected        = errors.New("type cannot be unset or blanked")
	ErrInvalidWriteArgs     = errors.New("invalid okf_write arguments")
	ErrConflict             = errors.New("the file changed since you read it; read it again and retry with its current digest")
	ErrChangedSincePreview  = errors.New("concept changed since the preview")
)
