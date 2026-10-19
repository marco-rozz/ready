package spec

type ToolOutputKind string

const (
	ToolOutputKindNone  ToolOutputKind = "none"
	ToolOutputKindText  ToolOutputKind = "text"
	ToolOutputKindImage ToolOutputKind = "image"
	ToolOutputKindFile  ToolOutputKind = "file"
)

type ToolOutputText struct {
	Text string `json:"text"`
}

type ImageDetail string

const (
	ImageDetailHigh ImageDetail = "high"
	ImageDetailLow  ImageDetail = "low"
	ImageDetailAuto ImageDetail = "auto"
)

type ToolOutputImage struct {
	Detail    ImageDetail `json:"detail"`
	ImageName string      `json:"imageName"`
	ImageMIME string      `json:"imageMIME"`
	ImageData string      `json:"imageData"`
}

type ToolOutputFile struct {
	FileName string `json:"fileName"`
	FileMIME string `json:"fileMIME"`
	FileData string `json:"fileData"`
}

type ToolOutputUnion struct {
	Kind ToolOutputKind `json:"kind"`

	TextItem  *ToolOutputText  `json:"textItem,omitempty"`
	ImageItem *ToolOutputImage `json:"imageItem,omitempty"`
	FileItem  *ToolOutputFile  `json:"fileItem,omitempty"`
}
