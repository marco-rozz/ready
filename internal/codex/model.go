// Package codex adapts the experimental ChatGPT Codex Responses wire
// contract to Eino's model.AgenticModel boundary. Authentication, endpoint
// scoping, and request policy remain READY-owned; generic Responses
// message/event conversion is delegated to the existing Eino component.
package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	schemaopenai "github.com/cloudwego/eino/schema/openai"
	"github.com/marco-rozz/ready/internal/codexauth"
	"github.com/openai/openai-go/v3/responses"
)

const (
	DefaultBaseURL = "https://chatgpt.com/backend-api"
	responsesPath  = "/codex"
	readyUserAgent = "READY/openai-codex"
)

type Config struct {
	BaseURL     string
	Model       string
	TokenSource codexauth.AccessSource
	HTTPClient  *http.Client
}

// Model is READY's Codex-specific AgenticModel. The inner Eino Responses
// component is an implementation detail behind this provider boundary.
type Model struct {
	inner model.AgenticModel
}

var _ model.AgenticModel = (*Model)(nil)

func NewModel(ctx context.Context, cfg Config) (*Model, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("create codex model: model ID is required")
	}
	if cfg.TokenSource == nil {
		return nil, fmt.Errorf("create codex model: token source is required")
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	origin, err := url.Parse(baseURL)
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		return nil, fmt.Errorf("create codex model: invalid base URL")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	} else {
		clone := *client
		client = &clone
	}
	baseTransport := client.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	client.Transport = &authTransport{base: baseTransport, origin: origin, source: cfg.TokenSource}

	store := false
	parallel := true
	maxRetries := 0
	inner, err := agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{
		BaseURL:           baseURL + responsesPath,
		Model:             cfg.Model,
		HTTPClient:        client,
		MaxRetries:        &maxRetries,
		Store:             &store,
		ParallelToolCalls: &parallel,
		Include: []responses.ResponseIncludable{
			responses.ResponseIncludable("reasoning.encrypted_content"),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create codex responses adapter: %w", err)
	}

	return &Model{inner: inner}, nil
}

func (m *Model) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	stream, err := m.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var chunks []*schema.AgenticMessage
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return nil, recvErr
		}
		chunks = append(chunks, chunk)
	}
	msg, err := schema.ConcatAgenticMessages(chunks)
	if err != nil {
		return nil, fmt.Errorf("concat codex response stream: %w", err)
	}

	return msg, nil
}

func (m *Model) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	prepared, callOpts, err := prepare(input, opts)
	if err != nil {
		return nil, err
	}
	stream, err := m.inner.Stream(ctx, prepared, callOpts...)
	if err != nil {
		return nil, fmt.Errorf("codex responses stream failed: %w", err)
	}

	return validateStream(stream), nil
}

func validateStream(inner *schema.StreamReader[*schema.AgenticMessage]) *schema.StreamReader[*schema.AgenticMessage] {
	out, writer := schema.Pipe[*schema.AgenticMessage](1)
	go func() {
		defer writer.Close()
		defer inner.Close()
		terminal := false
		for {
			chunk, err := inner.Recv()
			if errors.Is(err, io.EOF) {
				if !terminal {
					_ = writer.Send(nil, fmt.Errorf("codex protocol error: stream ended without a terminal response"))
				}

				return
			}
			if err != nil {
				_ = writer.Send(nil, fmt.Errorf("codex response stream failed: %w", err))

				return
			}
			if chunk != nil && chunk.ResponseMeta != nil && chunk.ResponseMeta.OpenAIExtension != nil {
				ext := chunk.ResponseMeta.OpenAIExtension
				switch ext.Status {
				case "", schemaopenai.ResponseStatusQueued, schemaopenai.ResponseStatusInProgress:
				case schemaopenai.ResponseStatusCompleted:
					terminal = true
				case schemaopenai.ResponseStatusFailed, schemaopenai.ResponseStatusCancelled, schemaopenai.ResponseStatusIncomplete:
					code := "unknown"
					if ext.Error != nil && ext.Error.Code != "" {
						code = sanitizeProtocolCode(string(ext.Error.Code))
					}
					_ = writer.Send(nil, fmt.Errorf("codex response failed: status=%s code=%s", ext.Status, code))

					return
				default:
					_ = writer.Send(nil, fmt.Errorf("codex protocol error: unsupported terminal status %q", ext.Status))

					return
				}
			}
			if writer.Send(chunk, nil) {
				return
			}
			// A completed response is the sole successful terminal outcome. Do
			// not wait for the HTTP body to close (or accept later events) after
			// the terminal event has been observed and delivered.
			if terminal {
				return
			}
		}
	}()

	return out
}

func sanitizeProtocolCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}

	return b.String()
}

// prepare maps system blocks to the Codex top-level instructions field while
// preserving the remaining ordered input items. It also supplies automatic
// tool choice unless the caller explicitly selected another mode.
func prepare(input []*schema.AgenticMessage, opts []model.Option) ([]*schema.AgenticMessage, []model.Option, error) {
	prepared := make([]*schema.AgenticMessage, 0, len(input))
	var instructions []string
	for _, msg := range input {
		if msg == nil {
			continue
		}
		if msg.Role != schema.AgenticRoleTypeSystem {
			prepared = append(prepared, msg)

			continue
		}
		for _, block := range msg.ContentBlocks {
			if block == nil || block.Type != schema.ContentBlockTypeUserInputText || block.UserInputText == nil {
				return nil, nil, fmt.Errorf("prepare codex request: unsupported system content block")
			}
			if block.UserInputText.Text != "" {
				instructions = append(instructions, block.UserInputText.Text)
			}
		}
	}
	callOpts := append([]model.Option(nil), opts...)
	callOpts = append(callOpts, agenticopenai.WithExtraFields(map[string]any{
		"instructions": strings.Join(instructions, "\n\n"),
	}))
	common := model.GetCommonOptions(nil, opts...)
	if common.AgenticToolChoice == nil {
		callOpts = append(callOpts, model.WithAgenticToolChoice(&schema.AgenticToolChoice{Type: schema.ToolChoiceAllowed}))
	}

	return prepared, callOpts, nil
}
