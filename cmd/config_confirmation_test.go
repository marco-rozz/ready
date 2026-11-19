//nolint:goconst // channel metadata literals make the test cases explicit
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/tools"
)

func TestConfirmationUsesActualUserInputAcrossChannels(t *testing.T) {
	for _, transport := range []string{"zmq", "telegram"} {
		t.Run(transport, func(t *testing.T) {
			var ch channel.Channel = &termZMQChannel{}
			msg := channel.Message{ChannelName: "cli", Metadata: map[string]string{"transport": "zmq"}}
			if transport == "telegram" {
				ch = &termTGChannel{}
				msg = channel.Message{ChannelName: "telegram", Metadata: map[string]string{"chat_id": "123"}}
			}
			env := newTermEnv(t, channelFormatterCfg{}, ch)
			ctx := context.Background()
			cur, _ := env.sessions.History(ctx, session.AddressOf(msg))
			path := writeTempConfig(t, fmt.Sprintf(zmqBootConfig, "!secret zmqcli_auth_token"))
			mgr, err := tools.NewConfigManager(tools.ConfigManagerOptions{
				ElectedPath: path,
				SessionIdentity: func(ctx context.Context) (string, bool) {
					id, ok := ctx.Value(toolSessionKeyCtxKey{}).(string)

					return id, ok
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			tool := tools.NewConfigManagerTool(mgr)
			toolCtx := context.WithValue(ctx, toolSessionKeyCtxKey{}, cur.ID)
			out, err := tool.InvokableRun(toolCtx, `{"action":"begin"}`)
			if err != nil {
				t.Fatal(err)
			}
			var draft struct {
				ID string `json:"stage_id"`
			}
			if err = json.Unmarshal([]byte(out), &draft); err != nil {
				t.Fatal(err)
			}
			update := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":"gemma4:31b","new_text":"gemma4:27b"}]}`, draft.ID)
			if _, err = tool.InvokableRun(toolCtx, update); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), "127.0.0.1:7600", "127.0.0.1:7601", 1))
			if err = os.WriteFile(path, data, 0o600); err != nil { // #nosec G703 -- path comes from t.TempDir
				t.Fatal(err)
			}
			save := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":1}`, draft.ID)
			if _, err = tool.InvokableRun(toolCtx, save); !errors.Is(err, config.ErrConfigSourceConflict) {
				t.Fatalf("conflict: %v", err)
			}
			reconcile := fmt.Sprintf(`{"action":"reconcile","stage_id":%q,"expected_revision":1}`, draft.ID)
			out, err = tool.InvokableRun(toolCtx, reconcile)
			if err != nil {
				t.Fatal(err)
			}
			var proposal struct {
				ID       string `json:"proposal_id"`
				Revision int    `json:"revision"`
			}
			if err = json.Unmarshal([]byte(out), &proposal); err != nil {
				t.Fatal(err)
			}
			save = fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":%d}`, draft.ID, proposal.Revision)
			rt := env.runtime()
			rt.cfgMgr = mgr
			msg.Content = "/confirm-config " + proposal.ID
			env.run(msg) // the inactive profile cannot authorize confirmation
			if _, err = tool.InvokableRun(toolCtx, save); err == nil {
				t.Fatal("disabled profile recorded confirmation")
			}
			rt.configManagerAllowed = true
			env.run(msg)
			if _, err = tool.InvokableRun(toolCtx, save); err != nil {
				t.Fatalf("trusted confirmation: %v", err)
			}
		})
	}
}

func TestTUIAndTelegramHaveIndependentDrafts(t *testing.T) {
	ctx := context.Background()
	env := newTermEnv(t, channelFormatterCfg{}, &termZMQChannel{}, &termTGChannel{})
	cli := channel.Message{ChannelName: "cli", Metadata: map[string]string{"transport": "zmq"}}
	tg := channel.Message{ChannelName: "telegram", Metadata: map[string]string{"chat_id": "123"}}
	cliSession, _ := env.sessions.History(ctx, session.AddressOf(cli))
	tgSession, _ := env.sessions.History(ctx, session.AddressOf(tg))
	path := writeTempConfig(t, fmt.Sprintf(zmqBootConfig, "!secret zmqcli_auth_token"))
	mgr, err := tools.NewConfigManager(tools.ConfigManagerOptions{
		ElectedPath: path,
		SessionIdentity: func(ctx context.Context) (string, bool) {
			id, ok := ctx.Value(toolSessionKeyCtxKey{}).(string)

			return id, ok
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tool := tools.NewConfigManagerTool(mgr)
	ids := make(map[string]bool)
	for _, cur := range []*session.Session{cliSession, tgSession} {
		toolCtx := context.WithValue(ctx, toolSessionKeyCtxKey{}, cur.ID)
		out, beginErr := tool.InvokableRun(toolCtx, `{"action":"begin"}`)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		var view struct {
			ID string `json:"stage_id"`
		}
		if err = json.Unmarshal([]byte(out), &view); err != nil {
			t.Fatal(err)
		}
		ids[view.ID] = true
	}
	if len(ids) != 2 {
		t.Fatal("TUI and Telegram shared a draft")
	}
	// A Connection loss does not end a Session: reusing its trusted ID keeps
	// access. Ending that Session releases only its own draft.
	cliCtx := context.WithValue(ctx, toolSessionKeyCtxKey{}, cliSession.ID)
	tgCtx := context.WithValue(ctx, toolSessionKeyCtxKey{}, tgSession.ID)
	if _, err = tool.InvokableRun(cliCtx, `{"action":"inspect"}`); err != nil {
		t.Fatal(err)
	}
	env.runtime().cfgMgr = mgr
	env.runtime().onSessionEnd(ctx, cliSession)
	if _, err = tool.InvokableRun(cliCtx, `{"action":"inspect"}`); err == nil {
		t.Fatal("ended Session kept its draft")
	}
	if _, err = tool.InvokableRun(tgCtx, `{"action":"inspect"}`); err != nil {
		t.Fatalf("other Session lost draft: %v", err)
	}
}
