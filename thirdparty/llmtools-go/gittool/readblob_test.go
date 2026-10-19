package gittool

import (
	"encoding/base64"
	"path/filepath"
	"testing"
)

func TestReadBlobRejectsInvalidEncoding(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "blob fixtures")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.ReadBlob(ctx, ReadBlobArgs{
		RepoPath: repoRel,
		Path:     testFileName,
		Encoding: BlobEncoding("bogus"),
	})
	if err == nil {
		t.Fatal("ReadBlob(invalid encoding) error = nil, want error")
	}
}

func TestReadBlobHandlesTextBinaryAndTruncation(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	mustWriteBinaryFile(t, repoAbs, testBinaryFileName, []byte(testBinaryContent))
	_ = mustCommitAll(t, repo, "blob fixtures")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	textOut, err := tool.ReadBlob(ctx, ReadBlobArgs{
		RepoPath: repoRel,
		Path:     testFileName,
	})
	if err != nil {
		t.Fatalf("ReadBlob(text auto) error = %v", err)
	}
	if textOut.Encoding != BlobEncodingText {
		t.Fatalf("ReadBlob(text auto).Encoding = %q, want %q", textOut.Encoding, BlobEncodingText)
	}
	if textOut.IsBinary {
		t.Fatal("ReadBlob(text auto).IsBinary = true, want false")
	}
	if textOut.Content != testFileContent {
		t.Fatalf("ReadBlob(text auto).Content = %q, want %q", textOut.Content, testFileContent)
	}
	if textOut.Truncated {
		t.Fatal("ReadBlob(text auto).Truncated = true, want false")
	}

	truncatedOut, err := tool.ReadBlob(ctx, ReadBlobArgs{
		RepoPath: repoRel,
		Path:     testFileName,
		MaxBytes: 3,
	})
	if err != nil {
		t.Fatalf("ReadBlob(truncated) error = %v", err)
	}
	if !truncatedOut.Truncated {
		t.Fatal("ReadBlob(truncated).Truncated = false, want true")
	}
	if truncatedOut.Content != "alp" {
		t.Fatalf("ReadBlob(truncated).Content = %q, want %q", truncatedOut.Content, "alp")
	}
	if truncatedOut.Bytes != 3 {
		t.Fatalf("ReadBlob(truncated).Bytes = %d, want 3", truncatedOut.Bytes)
	}

	binaryAuto, err := tool.ReadBlob(ctx, ReadBlobArgs{
		RepoPath: repoRel,
		Path:     testBinaryFileName,
	})
	if err != nil {
		t.Fatalf("ReadBlob(binary auto) error = %v", err)
	}
	if !binaryAuto.IsBinary {
		t.Fatal("ReadBlob(binary auto).IsBinary = false, want true")
	}
	if !binaryAuto.ContentOmitted {
		t.Fatal("ReadBlob(binary auto).ContentOmitted = false, want true")
	}
	if binaryAuto.Encoding != BlobEncodingAuto {
		t.Fatalf("ReadBlob(binary auto).Encoding = %q, want %q", binaryAuto.Encoding, BlobEncodingAuto)
	}

	binaryBase64, err := tool.ReadBlob(ctx, ReadBlobArgs{
		RepoPath: repoRel,
		Path:     testBinaryFileName,
		Encoding: BlobEncodingBase64,
	})
	if err != nil {
		t.Fatalf("ReadBlob(binary base64) error = %v", err)
	}
	if binaryBase64.Encoding != BlobEncodingBase64 {
		t.Fatalf("ReadBlob(binary base64).Encoding = %q, want %q", binaryBase64.Encoding, BlobEncodingBase64)
	}
	if binaryBase64.Content != base64.StdEncoding.EncodeToString([]byte(testBinaryContent)) {
		t.Fatalf("ReadBlob(binary base64).Content = %q, want base64 encoding", binaryBase64.Content)
	}
	if !binaryBase64.IsBinary {
		t.Fatal("ReadBlob(binary base64).IsBinary = false, want true")
	}

	_, err = tool.ReadBlob(ctx, ReadBlobArgs{
		RepoPath: repoRel,
		Path:     testBinaryFileName,
		Encoding: BlobEncodingText,
	})
	if err == nil {
		t.Fatal("ReadBlob(binary text) error = nil, want error")
	}
}

func TestReadBlobMissingPath(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "blob fixtures")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.ReadBlob(ctx, ReadBlobArgs{RepoPath: repoRel, Path: "missing.txt"})
	if err == nil {
		t.Fatal("ReadBlob(missing) error = nil, want error")
	}
}
