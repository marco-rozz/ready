package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/codexauth"
	"github.com/marco-rozz/ready/internal/config"
	homemcp "github.com/marco-rozz/ready/internal/mcp"
	"github.com/marco-rozz/ready/internal/oauth"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/secretstore"
)

type activeOAuthOp struct {
	mu            sync.Mutex
	completed     bool
	owner         string
	opID          string
	target        string
	providerName  string
	acceptsManual bool
	expectedState string
	verifier      string
	snapshot      int64
	codexClient   *codexauth.Client
	codexStore    *codexauth.SecretStore
	loopbackSrv   *codexauth.LoopbackServer
	cancel        context.CancelFunc
	done          chan struct{}
}

func (op *activeOAuthOp) claimCompletion() bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.completed {
		return false
	}
	op.completed = true

	return true
}

// oauthLoginService coordinates interactive OAuth logins over ZMQ for a
// numeric-loopback client (`ready chat`) without the client touching
// `<agent-dir>` on disk.
type oauthLoginService struct {
	parentCtx context.Context
	store     *secretstore.Store
	cfgFn     func(context.Context) *config.Config
	onResult  func(connectionID string, resp protocol.Response)
	onChange  func(context.Context)

	// Test seams.
	newCodexClient     func() *codexauth.Client
	startCodexLoopback func(ctx context.Context, expectedState string) (*codexauth.LoopbackServer, error)

	mu     sync.Mutex
	active *activeOAuthOp
}

func newOAuthLoginService(
	parentCtx context.Context,
	cfgFn func(context.Context) *config.Config,
	store *secretstore.Store,
	onResult func(connectionID string, resp protocol.Response),
	onChange func(context.Context),
) *oauthLoginService {
	return &oauthLoginService{
		parentCtx: parentCtx,
		store:     store,
		cfgFn:     cfgFn,
		onResult:  onResult,
		onChange:  onChange,
		newCodexClient: func() *codexauth.Client {
			return codexauth.NewClient(&http.Client{Timeout: 30 * time.Second})
		},
		startCodexLoopback: codexauth.StartLoopbackServer,
	}
}

func buildOAuthLoginHandler(svc *oauthLoginService, allowedFn func() bool) zmqcli.OAuthLoginHandler {
	return func(hctx context.Context, _ []byte, loopback bool, req protocol.Request) protocol.Response {
		if allowedFn == nil || !allowedFn() {
			return protocol.Response{
				Status:       protocol.ResponseStatusError,
				ResponseType: protocol.ResponseTypeReply,
				ErrorCode:    protocol.ErrorCodeProfileRequired,
			}
		}
		if !loopback {
			return protocol.Response{
				Status:       protocol.ResponseStatusError,
				ResponseType: protocol.ResponseTypeReply,
				ErrorCode:    protocol.ErrorCodeLoopbackRequired,
			}
		}
		payload := req.OAuthLogin
		if payload == nil {
			return protocol.Response{
				Status:       protocol.ResponseStatusError,
				ResponseType: protocol.ResponseTypeReply,
				ErrorCode:    protocol.ErrorCodeMalformedRequest,
			}
		}
		owner := req.ConnectionID
		switch payload.Action {
		case protocol.OAuthLoginActionBegin:
			return svc.Begin(hctx, owner, payload.Target)
		case protocol.OAuthLoginActionSubmit:
			val := ""
			if payload.Value != nil {
				val = *payload.Value
			}

			return svc.Submit(hctx, owner, payload.OperationID, payload.Target, val)
		case protocol.OAuthLoginActionCancel:
			return svc.Cancel(hctx, owner, payload.OperationID, payload.Target)
		default:
			return protocol.Response{
				Status:       protocol.ResponseStatusError,
				ResponseType: protocol.ResponseTypeReply,
				ErrorCode:    protocol.ErrorCodeMalformedRequest,
			}
		}
	}
}

func (s *oauthLoginService) Begin(ctx context.Context, owner, target string) protocol.Response {
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()

		return protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeReply,
			ErrorCode:    protocol.ErrorCodeSecretEntryActive,
		}
	}
	s.mu.Unlock()

	kind, name, ok := strings.Cut(target, ":")
	if !ok || name == "" {
		return protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeReply,
			ErrorCode:    protocol.ErrorCodeMalformedRequest,
		}
	}

	cfg := s.cfgFn(ctx)
	if cfg == nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "configuration unavailable")
	}

	switch kind {
	case "llm":
		return s.beginCodex(ctx, cfg, owner, target, name)
	case "mcp":
		return s.beginMCP(ctx, cfg, owner, target, name)
	default:
		return protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeReply,
			ErrorCode:    protocol.ErrorCodeUnknownName,
		}
	}
}

func (s *oauthLoginService) beginCodex(ctx context.Context, cfg *config.Config, owner, target, name string) protocol.Response {
	prov, exists := cfg.LLM.Providers[name]
	if !exists || prov.Provider != config.ProviderOpenAICodex {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeUnknownName, "unknown or non-OAuth LLM provider")
	}

	verifier, challenge, err := codexauth.NewPKCE()
	if err != nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "failed to initialize PKCE")
	}
	state, err := codexauth.NewState()
	if err != nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "failed to initialize OAuth state")
	}
	client := s.newCodexClient()
	authURL, err := client.AuthorizationURL(state, challenge)
	if err != nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "failed to build authorization URL")
	}

	codexStore := codexauth.NewSecretStore(s.store, cfg.OAuth.Path)
	snapshot, err := codexStore.Snapshot(ctx, name)
	if err != nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "failed to snapshot credential store")
	}

	flowCtx, flowCancel := context.WithCancel(s.parentCtx)
	srv, err := s.startCodexLoopback(flowCtx, state)
	if err != nil {
		flowCancel()

		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "failed to bind OAuth callback port 1455 (is another login running?)")
	}

	opID := "oauth-" + uuid.New().String()
	done := make(chan struct{})
	op := &activeOAuthOp{
		owner:         owner,
		opID:          opID,
		target:        target,
		providerName:  name,
		acceptsManual: true,
		expectedState: state,
		verifier:      verifier,
		snapshot:      snapshot,
		codexClient:   client,
		codexStore:    codexStore,
		loopbackSrv:   srv,
		cancel:        flowCancel,
		done:          done,
	}

	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		flowCancel()
		srv.Close()

		return protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeReply,
			ErrorCode:    protocol.ErrorCodeSecretEntryActive,
		}
	}
	s.active = op
	s.mu.Unlock()

	go func() {
		defer close(done)
		res, waitErr := srv.Wait(flowCtx, codexauth.DefaultFlowTimeout)
		s.finishCodexCallback(flowCtx, op, res, waitErr)
	}()

	return protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		OAuthLogin: &protocol.OAuthLoginResponse{
			Action:        protocol.OAuthLoginActionBegin,
			OperationID:   opID,
			Target:        target,
			Outcome:       protocol.OAuthLoginOutcomeWaiting,
			AuthURL:       authURL,
			AcceptsManual: true,
		},
	}
}

func (s *oauthLoginService) finishCodexCallback(flowCtx context.Context, op *activeOAuthOp, res codexauth.AuthorizationResult, waitErr error) {
	if !op.claimCompletion() {
		return
	}
	s.clearActive(op.opID)
	defer op.cancel()

	if waitErr != nil {
		s.emitResult(op.owner, protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeControl,
			Content:      "OAuth callback failed or timed out",
			ErrorCode:    protocol.ErrorCodeWriteNotCommitted,
			OAuthLogin: &protocol.OAuthLoginResponse{
				OperationID: op.opID,
				Target:      op.target,
				Outcome:     protocol.OAuthLoginOutcomeRefused,
			},
		})

		return
	}

	exCtx, cancel := context.WithTimeout(flowCtx, 30*time.Second)
	defer cancel()
	cred, err := op.codexClient.ExchangeCode(exCtx, op.providerName, res.Code, op.verifier)
	if err == nil {
		err = op.codexStore.SaveCommitted(exCtx, cred, op.snapshot)
	}
	if err != nil {
		s.emitResult(op.owner, protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeControl,
			Content:      "OAuth token exchange failed",
			ErrorCode:    protocol.ErrorCodeWriteNotCommitted,
			OAuthLogin: &protocol.OAuthLoginResponse{
				OperationID: op.opID,
				Target:      op.target,
				Outcome:     protocol.OAuthLoginOutcomeRefused,
			},
		})

		return
	}

	if s.onChange != nil {
		s.onChange(exCtx)
	}
	s.emitResult(op.owner, protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		Content:      protocol.ControlOAuthLoginResult,
		OAuthLogin: &protocol.OAuthLoginResponse{
			OperationID: op.opID,
			Target:      op.target,
			Outcome:     protocol.OAuthLoginOutcomeCommitted,
		},
	})
}

func (s *oauthLoginService) beginMCP(ctx context.Context, cfg *config.Config, owner, target, name string) protocol.Response {
	srv, exists := cfg.MCPServers[name]
	if !exists || srv.OAuth == nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeUnknownName, "unknown or non-OAuth MCP server")
	}

	if pending, err := srv.URL.Resolve(ctx, cfg, "mcp_servers."+name+".url"); err != nil || pending || srv.URL.Value == "" {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, fmt.Sprintf("mcp_servers.%s.url is pending or invalid", name))
	}
	_, _ = srv.ResolveHeaders(ctx, cfg.SecretValues, name)
	httpClient := homemcp.NewScopedHTTPClient(&srv)

	challenges, err := oauth.Probe(ctx, httpClient, srv.URL.Value)
	if err != nil {
		if errors.Is(err, oauth.ErrNoAuthorizationRequired) {
			return protocol.Response{
				Status:       protocol.ResponseStatusOK,
				ResponseType: protocol.ResponseTypeReply,
				OAuthLogin: &protocol.OAuthLoginResponse{
					Action:  protocol.OAuthLoginActionBegin,
					Target:  target,
					Outcome: protocol.OAuthLoginOutcomeNotRequired,
				},
			}
		}

		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "MCP OAuth probe failed")
	}

	prm, err := oauth.DiscoverProtectedResource(ctx, httpClient, challenges, srv.URL.Value)
	if err != nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "MCP protected resource discovery failed")
	}

	asm, err := oauth.DiscoverAuthServer(ctx, httpClient, prm)
	if err != nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "MCP authorization server discovery failed")
	}

	secret, secretOK, err := cfg.ResolveSecret(ctx, srv.OAuth.ClientSecret, "mcp_servers."+name+".oauth.client_secret")
	if err != nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "failed to resolve MCP client secret")
	}

	hasClientCredentials := srv.OAuth.ClientID != "" && secretOK && secret != ""
	authServerSupportsCC := slices.Contains(asm.GrantTypesSupported, "client_credentials")
	grant := oauth.ResolveGrant(srv.OAuth.Grant, hasClientCredentials, authServerSupportsCC)
	if grant == oauth.GrantClientCredentials {
		return protocol.Response{
			Status:       protocol.ResponseStatusOK,
			ResponseType: protocol.ResponseTypeReply,
			OAuthLogin: &protocol.OAuthLoginResponse{
				Action:  protocol.OAuthLoginActionBegin,
				Target:  target,
				Outcome: protocol.OAuthLoginOutcomeNotRequired,
			},
		}
	}

	mcpStore := oauth.NewSecretStore(s.store, cfg.OAuth.Path)
	existing, err := mcpStore.Load(ctx, name)
	if err != nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "failed to load stored MCP credential")
	}
	snapshot, err := mcpStore.Snapshot(ctx, name)
	if err != nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "failed to snapshot stored MCP credential")
	}
	var guardedStore oauth.Store = &generationGuardedStore{inner: mcpStore, name: name, snapshot: snapshot}

	redirectPort := cfg.OAuth.RedirectPort
	if redirectPort <= 0 {
		redirectPort = 7601
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", redirectPort)
	if existing != nil && existing.RedirectURI != "" && existing.RedirectURI != redirectURI {
		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, redirectMismatch(existing, redirectURI, cfg.RuntimeHome).Error())
	}

	opID := "oauth-" + uuid.New().String()
	flowCtx, flowCancel := context.WithCancel(s.parentCtx)
	done := make(chan struct{})
	urlCh := make(chan string, 1)
	earlyErrCh := make(chan error, 1)

	op := &activeOAuthOp{
		owner:         owner,
		opID:          opID,
		target:        target,
		acceptsManual: false,
		cancel:        flowCancel,
		done:          done,
	}

	fetcher := oauth.InteractiveFetcherWithOpener(redirectPort, oauth.DefaultAuthorizationTimeout, func(_ context.Context, authURL string) error {
		select {
		case urlCh <- authURL:
		default:
		}

		return nil
	})

	go func() {
		defer close(done)
		var fetcherErr error
		trackedFetcher := trackingFetcher(fetcher, &fetcherErr)
		var connectErr error
		curExisting := existing
		for attempt := range 2 {
			client, retry, aerr := runAuthorizationCodeAttempt(
				flowCtx, cfg, guardedStore, name, srv, httpClient, asm, prm,
				curExisting, redirectURI, secret, trackedFetcher, io.Discard, attempt == 0,
			)
			if client != nil {
				connectErr = nil

				break
			}
			connectErr = classifyLoginConnectErr(aerr, fetcherErr)
			if !retry {
				break
			}
			curExisting = nil
			fetcherErr = nil
		}
		if connectErr != nil {
			select {
			case earlyErrCh <- connectErr:
			default:
			}
		}
		s.finishMCPCallback(flowCtx, op, connectErr)
	}()

	select {
	case authURL := <-urlCh:
		s.mu.Lock()
		if s.active != nil {
			s.mu.Unlock()
			op.claimCompletion()
			flowCancel()
			<-done

			return protocol.Response{
				Status:       protocol.ResponseStatusError,
				ResponseType: protocol.ResponseTypeReply,
				ErrorCode:    protocol.ErrorCodeSecretEntryActive,
			}
		}
		s.active = op
		s.mu.Unlock()

		return protocol.Response{
			Status:       protocol.ResponseStatusOK,
			ResponseType: protocol.ResponseTypeReply,
			OAuthLogin: &protocol.OAuthLoginResponse{
				Action:        protocol.OAuthLoginActionBegin,
				OperationID:   opID,
				Target:        target,
				Outcome:       protocol.OAuthLoginOutcomeWaiting,
				AuthURL:       authURL,
				AcceptsManual: false,
			},
		}
	case earlyErr := <-earlyErrCh:
		op.claimCompletion()
		flowCancel()
		<-done

		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, earlyErr.Error())
	case <-ctx.Done():
		op.claimCompletion()
		flowCancel()
		<-done

		return oauthRefusedResponse(protocol.OAuthLoginActionBegin, "", target, protocol.ErrorCodeWriteNotCommitted, "OAuth login begin timed out")
	}
}

func (s *oauthLoginService) finishMCPCallback(flowCtx context.Context, op *activeOAuthOp, connectErr error) {
	if !op.claimCompletion() {
		return
	}
	s.clearActive(op.opID)
	defer op.cancel()

	if connectErr != nil {
		s.emitResult(op.owner, protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeControl,
			Content:      "MCP OAuth authorization failed",
			ErrorCode:    protocol.ErrorCodeWriteNotCommitted,
			OAuthLogin: &protocol.OAuthLoginResponse{
				OperationID: op.opID,
				Target:      op.target,
				Outcome:     protocol.OAuthLoginOutcomeRefused,
			},
		})

		return
	}

	if s.onChange != nil {
		s.onChange(flowCtx)
	}
	s.emitResult(op.owner, protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		Content:      protocol.ControlOAuthLoginResult,
		OAuthLogin: &protocol.OAuthLoginResponse{
			OperationID: op.opID,
			Target:      op.target,
			Outcome:     protocol.OAuthLoginOutcomeCommitted,
		},
	})
}

func (s *oauthLoginService) Submit(ctx context.Context, owner, opID, target, rawValue string) protocol.Response {
	s.mu.Lock()
	op := s.active
	if op == nil || op.owner != owner || op.opID != opID || op.target != target {
		s.mu.Unlock()

		return protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeReply,
			ErrorCode:    protocol.ErrorCodeSecretEntryExpired,
		}
	}
	if !op.acceptsManual {
		s.mu.Unlock()

		return oauthRefusedResponse(protocol.OAuthLoginActionSubmit, opID, target, protocol.ErrorCodeInvalidValue, "target does not accept manual callback submission")
	}
	res, parseErr := codexauth.ParseManualCallback(rawValue, op.expectedState)
	if parseErr != nil {
		s.mu.Unlock()

		return oauthRefusedResponse(protocol.OAuthLoginActionSubmit, opID, target, protocol.ErrorCodeInvalidValue, parseErr.Error())
	}
	if !op.claimCompletion() {
		s.mu.Unlock()

		return protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeReply,
			ErrorCode:    protocol.ErrorCodeSecretEntryExpired,
		}
	}
	s.active = nil
	s.mu.Unlock()

	op.cancel()
	if op.loopbackSrv != nil {
		op.loopbackSrv.Close()
	}
	waitOpDone(op.done)

	cred, err := op.codexClient.ExchangeCode(ctx, op.providerName, res.Code, op.verifier)
	if err == nil {
		err = op.codexStore.SaveCommitted(ctx, cred, op.snapshot)
	}
	if err != nil {
		return oauthRefusedResponse(protocol.OAuthLoginActionSubmit, opID, target, protocol.ErrorCodeWriteNotCommitted, "OAuth token exchange failed")
	}

	if s.onChange != nil {
		s.onChange(ctx)
	}

	return protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		OAuthLogin: &protocol.OAuthLoginResponse{
			Action:      protocol.OAuthLoginActionSubmit,
			OperationID: opID,
			Target:      target,
			Outcome:     protocol.OAuthLoginOutcomeCommitted,
		},
	}
}

func (s *oauthLoginService) Cancel(_ context.Context, owner, opID, target string) protocol.Response {
	s.mu.Lock()
	op := s.active
	if op == nil || op.owner != owner || op.opID != opID || op.target != target {
		s.mu.Unlock()

		return protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeReply,
			ErrorCode:    protocol.ErrorCodeSecretEntryExpired,
		}
	}
	op.claimCompletion()
	s.active = nil
	s.mu.Unlock()

	op.cancel()
	if op.loopbackSrv != nil {
		op.loopbackSrv.Close()
	}
	waitOpDone(op.done)

	return protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		OAuthLogin: &protocol.OAuthLoginResponse{
			Action:      protocol.OAuthLoginActionCancel,
			OperationID: opID,
			Target:      target,
			Outcome:     protocol.OAuthLoginOutcomeCancelled,
		},
	}
}

func (s *oauthLoginService) Release(owner string) {
	s.mu.Lock()
	op := s.active
	if op == nil || (owner != "" && op.owner != owner) {
		s.mu.Unlock()

		return
	}
	op.claimCompletion()
	s.active = nil
	s.mu.Unlock()

	op.cancel()
	if op.loopbackSrv != nil {
		op.loopbackSrv.Close()
	}
	waitOpDone(op.done)
}

func (s *oauthLoginService) clearActive(opID string) {
	s.mu.Lock()
	if s.active != nil && s.active.opID == opID {
		s.active = nil
	}
	s.mu.Unlock()
}

func (s *oauthLoginService) emitResult(owner string, resp protocol.Response) {
	if s.onResult != nil {
		s.onResult(owner, resp)
	}
}

func waitOpDone(done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

func oauthRefusedResponse(action protocol.OAuthLoginAction, opID, target, code, content string) protocol.Response {
	return protocol.Response{
		Status:       protocol.ResponseStatusError,
		ResponseType: protocol.ResponseTypeReply,
		ErrorCode:    code,
		Content:      content,
		OAuthLogin: &protocol.OAuthLoginResponse{
			Action:      action,
			OperationID: opID,
			Target:      target,
			Outcome:     protocol.OAuthLoginOutcomeRefused,
		},
	}
}
