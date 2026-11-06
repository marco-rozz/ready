package config

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"github.com/marco-rozz/ready/internal/secretstore"
)

// DefaultChatBind is the daemon's default ZMQ endpoint.
const DefaultChatBind = "tcp://127.0.0.1:7600"

// ChatAgent is what `ready chat` learns from a local agent: where its daemon
// listens and the shared secret to authenticate with.
type ChatAgent struct {
	// Address is the connectable endpoint (a wildcard bind maps to loopback).
	Address string
	// Token is the resolved HMAC shared secret.
	Token string
}

// chatView is the only part of the configuration `chat` reads. Every other key
// is ignored, so a configuration that fails full validation (or holds a
// literal elsewhere) never blocks a local chat.
type chatView struct {
	ConfigVersion int `yaml:"config_version"`
	Agent         struct {
		ID string `yaml:"id"`
	} `yaml:"agent"`
	Channels struct {
		ZMQCLI struct {
			Enabled   *bool     `yaml:"enabled"`
			Bind      string    `yaml:"bind"`
			AuthToken SecretRef `yaml:"auth_token"`
		} `yaml:"zmqcli"`
	} `yaml:"channels"`
	Secrets SecretsConfig `yaml:"secrets"`
}

// ResolveChatAgent reads the selected agent for `ready chat`: strictly
// read-only, no locks, no logging, nothing created. It parses only
// config_version, channels.zmqcli, agent.id and secrets, opens the Secret store
// read-only and reads the single referenced Secret.
func ResolveChatAgent(ctx context.Context, agentDirFlag string) (ChatAgent, error) {
	res, err := SelectAgentDir(ctx, agentDirFlag)
	if err != nil {
		return ChatAgent{}, err
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		return ChatAgent{}, fmt.Errorf("cannot read %s: %w", res.Path, err)
	}
	var v chatView
	if err = yaml.Unmarshal(data, &v); err != nil {
		return ChatAgent{}, fmt.Errorf("cannot read the chat settings of %s: %w", res.Path, err)
	}
	if v.ConfigVersion != TargetConfigVersion {
		return ChatAgent{}, fmt.Errorf("%s is at config_version %d, this client reads %d: upgrade by starting `ready serve`, or pass --token / --token-file", res.Path, v.ConfigVersion, TargetConfigVersion)
	}
	z := v.Channels.ZMQCLI
	if z.Enabled != nil && !*z.Enabled {
		return ChatAgent{}, errors.New("channels.zmqcli is disabled for this agent; there is no daemon to chat with")
	}
	if !z.AuthToken.IsSet() {
		return ChatAgent{}, errors.New("this agent configures no channels.zmqcli.auth_token; pass --token or --token-file")
	}

	var token string
	switch z.AuthToken.Kind {
	case SecretRefFile:
		value, ok, rerr := secretstore.ReadFileSecret(z.AuthToken.Path)
		if rerr != nil || !ok {
			return ChatAgent{}, fmt.Errorf("the chat secret file %s is missing or unreadable", z.AuthToken.Path)
		}
		token = value
	default:
		if v.Agent.ID == "" {
			return ChatAgent{}, ErrAgentIDMissing
		}
		store, serr := secretstore.New(secretstore.Options{Dir: res.AgentDir, Key: SecretsSpec(v.Secrets, v.Agent.ID), AgentID: v.Agent.ID, ReadOnly: true, Log: zerolog.Nop()})
		if serr != nil {
			return ChatAgent{}, serr
		}
		payload, verr := store.View(ctx)
		if errors.Is(verr, secretstore.ErrNoStore) {
			return ChatAgent{}, errors.New("the chat secret is not set yet: start `ready serve` once (it generates it)")
		}
		if verr != nil {
			return ChatAgent{}, fmt.Errorf("cannot read the secret store: %w", verr)
		}
		sec, ok := payload.Secrets[z.AuthToken.Name]
		if !ok || secretstore.CheckValue([]string{secretstore.FieldZMQAuthToken}, sec.Value) != nil {
			return ChatAgent{}, fmt.Errorf("the chat secret %q is not set yet: start `ready serve` once, or run `ready secrets set %s`", z.AuthToken.Name, z.AuthToken.Name)
		}
		token = sec.Value
	}

	bind := z.Bind
	if bind == "" {
		bind = DefaultChatBind
	}

	return ChatAgent{Address: ConnectableAddress(bind), Token: token}, nil
}

// SecretsSpec maps a secrets block and an agent id to the store's key location.
func SecretsSpec(s SecretsConfig, agentID string) secretstore.KeySpec {
	return secretstore.KeySpec{Location: secretstore.KeyLocation(s.StoreKey), File: s.StoreKeyFile, AgentID: agentID}
}

// ConnectableAddress turns a daemon's wildcard bind into an address a local
// client can dial.
func ConnectableAddress(bind string) string {
	if !strings.HasPrefix(bind, "tcp://") {
		return bind
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(bind, "tcp://"))
	if err != nil {
		return bind
	}
	switch host {
	case "0.0.0.0", "*", "":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}

	return "tcp://" + net.JoinHostPort(host, port)
}
