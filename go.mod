module github.com/marco-rozz/ready

go 1.27

// Pinned so CI (setup-go with go-version-file: go.mod), goreleaser and local
// builds all use the exact same Go patch release.
toolchain go1.27.4

require (
	charm.land/bubbles/v2 v2.2.2
	charm.land/bubbletea/v2 v2.1.2
	charm.land/glamour/v2 v2.0.1
	charm.land/lipgloss/v2 v2.1.1
	filippo.io/age v1.3.2
	github.com/charmbracelet/x/ansi v0.11.10
	github.com/charmbracelet/x/term v0.2.2
	github.com/cloudwego/eino v0.9.22
	github.com/cloudwego/eino-ext/components/model/agenticopenai v0.2.5
	github.com/cloudwego/eino-ext/components/model/claude v0.1.26
	github.com/cloudwego/eino-ext/components/model/gemini v0.1.37
	github.com/cloudwego/eino-ext/components/model/ollama v0.1.9
	github.com/cloudwego/eino-ext/components/model/openai v0.1.13
	github.com/cloudwego/eino-ext/components/model/openrouter v0.1.10
	github.com/creack/pty v1.1.24
	github.com/ebitengine/purego v0.11.3
	github.com/eino-contrib/jsonschema v1.0.3
	github.com/flexigpt/agentskills-go v0.22.2
	github.com/fsnotify/fsnotify v1.10.1
	github.com/go-telegram-bot-api/telegram-bot-api/v5 v5.5.1
	github.com/go-zeromq/zmq4 v0.17.0
	github.com/google/jsonschema-go v0.4.3
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674
	github.com/mattn/go-runewidth v0.0.31
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/muesli/cancelreader v0.2.2
	github.com/muesli/termenv v0.16.0
	github.com/openai/openai-go/v3 v3.50.0 // agenticopenai v0.2.5 is incompatible with openai-go releases newer than v3.50.0
	github.com/rs/zerolog v1.35.1
	github.com/urfave/cli/v3 v3.14.0
	github.com/wk8/go-ordered-map/v2 v2.1.8
	golang.org/x/oauth2 v0.38.0
	golang.org/x/sys v0.50.0
	golang.org/x/term v0.48.0
	golang.org/x/text v0.44.0
	google.golang.org/genai v1.79.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/auth v0.24.4 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.3.1 // indirect
	cloud.google.com/go/compute/metadata v0.10.0 // indirect
	filippo.io/hpke v0.4.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.24.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.13.0 // indirect
	github.com/alecthomas/chroma/v2 v2.27.0 // indirect
	github.com/anthropics/anthropic-sdk-go v1.84.0 // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aws/aws-sdk-go-v2 v1.48.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.27 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.33.21 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.21.3 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.9 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.25 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.14.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.39.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.44.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.53.0 // indirect
	github.com/aws/smithy-go v1.28.9 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.6.1 // indirect
	github.com/bytedance/sonic v1.15.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20261119151026-8ecbcf0e188c // indirect
	github.com/charmbracelet/x/exp/slice v0.1.0 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/cloudwego/eino-ext/libs/acl/openai v0.1.18-0.20260924031408-10df7cac3f6d // indirect
	github.com/dlclark/regexp2/v2 v2.13.2 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/eino-contrib/ollama v0.1.0 // indirect
	github.com/evanphx/json-patch v0.5.2 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/flexigpt/llmtools-go v0.25.2 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/go-zeromq/goczmq/v4 v4.2.2 // indirect
	github.com/goccy/go-yaml v1.19.2 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/s2a-go v0.1.11 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.25 // indirect
	github.com/googleapis/gax-go/v2 v2.27.0 // indirect
	github.com/goph/emperror v0.17.2 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/ledongthuc/pdf v0.0.0-20261020125853-5e46805f718e // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mailru/easyjson v0.9.2 // indirect
	github.com/mattn/go-colorable v0.1.16 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/meguminnnnnnnnn/go-openai v0.1.6 // indirect
	github.com/microcosm-cc/bluemonday v1.0.27 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/nikolalohinski/gonja v1.5.3 // indirect
	github.com/pb33f/go-yaml v0.2.0 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.2 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rivo/uniseg v0.4.8-0.20261110175422-3a370411ca06 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/sirupsen/logrus v1.10.2 // indirect
	github.com/slongfield/pyfmt v0.0.0-20220222012616-ea85ff4c361f // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v1.3.0 // indirect
	github.com/tidwall/gjson v1.20.6 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.3.1 // indirect
	github.com/tidwall/sjson v1.2.11 // indirect
	github.com/xo/terminfo v1.3.1 // indirect
	github.com/yargevad/filepathx v1.0.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/yuin/goldmark v1.8.7 // indirect
	github.com/yuin/goldmark-emoji v1.0.6 // indirect
	go.opentelemetry.io/auto/sdk v1.2.2 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.72.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	golang.org/x/crypto v0.59.0 // indirect
	golang.org/x/exp v0.0.0-20261124000051-d6646c509b6d // indirect
	golang.org/x/net v0.63.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	google.golang.org/api v0.316.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261123191846-1e707ab9fbc2 // indirect
	google.golang.org/grpc v1.85.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

// sonic v1.14.0+ introduced a compile-time 32-bit blocker constant that prevents
// cross-compilation for GOARCH=arm (Raspberry Pi 1/Zero). Replace with a local
// encoding/json shim that exposes only the API surface used by eino, allowing
// cross-compilation to any architecture without sacrificing correctness.
replace github.com/bytedance/sonic => ./sonic-shim

// Patched copy of llmtools-go whose built-in registry does not import
// webtool/go-dateparser/go-trafilatura or gittool/go-git — tools READY never
// registers as agent tools, worth ~12% of the release binary size.
// See thirdparty/README.md for the exact patch and refresh procedure.
replace github.com/flexigpt/llmtools-go => ./thirdparty/llmtools-go

// READY patches StreamReader to observe explicit consumer closure.
replace github.com/cloudwego/eino => ./thirdparty/eino

// READY bounds observational ROUTER writes and protects final-reply framing.
replace github.com/go-zeromq/zmq4 => ./thirdparty/zmq4
