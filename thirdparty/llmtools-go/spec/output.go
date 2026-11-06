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

// ToolOutputUnionListSchema returns a Draft-07 schema for the JSON encoding of
// []ToolOutputUnion.
func ToolOutputUnionListSchema() JSONSchema {
	return JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "array",
"items": {
	"oneOf": [
		{
			"type": "object",
			"properties": {
				"kind": {
					"const": "none"
				}
			},
			"required": ["kind"],
			"additionalProperties": false
		},
		{
			"type": "object",
			"properties": {
				"kind": {
					"const": "text"
				},
				"textItem": {
					"type": "object",
					"properties": {
						"text": {
							"type": "string"
						}
					},
					"required": ["text"],
					"additionalProperties": false
				}
			},
			"required": ["kind", "textItem"],
			"additionalProperties": false
		},
		{
			"type": "object",
			"properties": {
				"kind": {
					"const": "image"
				},
				"imageItem": {
					"type": "object",
					"properties": {
						"detail": {
							"type": "string",
							"enum": ["high", "low", "auto"]
						},
						"imageName": {
							"type": "string"
						},
						"imageMIME": {
							"type": "string"
						},
						"imageData": {
							"type": "string"
						}
					},
					"required": ["detail", "imageName", "imageMIME", "imageData"],
					"additionalProperties": false
				}
			},
			"required": ["kind", "imageItem"],
			"additionalProperties": false
		},
		{
			"type": "object",
			"properties": {
				"kind": {
					"const": "file"
				},
				"fileItem": {
					"type": "object",
					"properties": {
						"fileName": {
							"type": "string"
						},
						"fileMIME": {
							"type": "string"
						},
						"fileData": {
							"type": "string"
						}
					},
					"required": ["fileName", "fileMIME", "fileData"],
					"additionalProperties": false
				}
			},
			"required": ["kind", "fileItem"],
			"additionalProperties": false
		}
	]
}
}`)
}
