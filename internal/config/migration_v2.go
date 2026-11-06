package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/marco-rozz/ready/internal/secretstore"
)

// yamlStrTag is the tag of a plain string scalar.
const yamlStrTag = "!!str"

func nowUTC() time.Time { return time.Now().UTC() }

// migrationV2 is the v1→v2 step: sensitive fields become Secret references, the
// values move into the agent's Secret store, `*_file` keys and
// oauth.token_store disappear, and the agent identity is added.
type migrationV2 struct{}

var (
	_ Migration      = migrationV2{}
	_ SecretImporter = migrationV2{}
)

func (migrationV2) Version() int { return 2 }

func (migrationV2) Description() string {
	return "credentials become Secret references; add agent identity; drop *_file keys and oauth.token_store"
}

// Up performs the structural, value-free part: dropping the removed keys.
func (migrationV2) Up(doc *yaml.Node) error {
	root := documentRoot(doc)
	if root == nil {
		return fmt.Errorf("%w: the document root is not a mapping", ErrInvalidConfigDocument)
	}
	if oauth := mapValue(root, "oauth"); oauth != nil {
		removeKey(oauth, "token_store")
	}

	return nil
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	n := doc
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}

	return n
}

func mapIndex(m *yaml.Node, key string) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}

	return -1
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if i := mapIndex(m, key); i >= 0 {
		return m.Content[i+1]
	}

	return nil
}

func removeKey(m *yaml.Node, key string) {
	if i := mapIndex(m, key); i >= 0 {
		m.Content = append(m.Content[:i], m.Content[i+2:]...)
	}
}

// credSite is one parent mapping that carries a sensitive scalar, with the v1
// `*_file` companion key where it existed.
type credSite struct {
	parent  *yaml.Node
	path    []string // dotted-path segments of the parent
	key     string
	fileKey string
}

// sites lists every credential-bearing location of a v1 document.
func credentialSites(root *yaml.Node) []credSite {
	var out []credSite
	add := func(parent *yaml.Node, path []string, key, fileKey string) {
		if parent != nil && parent.Kind == yaml.MappingNode {
			out = append(out, credSite{parent: parent, path: path, key: key, fileKey: fileKey})
		}
	}
	each := func(m *yaml.Node, fn func(name string, v *yaml.Node)) {
		if m == nil || m.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			fn(m.Content[i].Value, m.Content[i+1])
		}
	}
	providers := mapValue(mapValue(root, segLLM), segProviders)
	each(providers, func(name string, v *yaml.Node) {
		add(v, []string{segLLM, segProviders, name}, segAPIKey, "api_key_file")
	})
	channels := mapValue(root, segChannels)
	add(mapValue(channels, segTelegram), []string{segChannels, segTelegram}, segToken, "")
	add(mapValue(channels, segZmqcli), []string{segChannels, segZmqcli}, segAuthToken, "auth_token_file")
	add(mapValue(mapValue(root, segHomeAssistant), segWebsocket), []string{segHomeAssistant, segWebsocket}, segToken, "token_file")
	each(mapValue(root, segMCPServers), func(name string, srv *yaml.Node) {
		add(mapValue(srv, segOAuth), []string{segMCPServers, name, segOAuth}, segClientSecret, "client_secret_file")
		headers := mapValue(srv, segHeaders)
		each(headers, func(h string, _ *yaml.Node) {
			add(headers, []string{segMCPServers, name, segHeaders}, h, "")
		})
	})
	sort.SliceStable(out, func(i, j int) bool {
		return strings.Join(out[i].path, ".")+out[i].key < strings.Join(out[j].path, ".")+out[j].key
	})

	return out
}

// refuseMarkers fails on any REDACTED- scalar anywhere in the document,
// naming the path and line, never the scalar.
func refuseMarkers(n *yaml.Node, path []string) error {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			if err := refuseMarkers(c, path); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if err := refuseMarkers(n.Content[i+1], append(append([]string{}, path...), n.Content[i].Value)); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			if err := refuseMarkers(c, append(append([]string{}, path...), fmt.Sprint(i))); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if strings.HasPrefix(n.Value, "REDACTED-") {
			return fmt.Errorf("%w: %s (line %d) still holds an unresolved REDACTED placeholder; replace it with the real credential (or remove it) and start again", ErrSecretImport, strings.Join(path, "."), n.Line)
		}
	}

	return nil
}

// PlanImport rewrites the credentials of a v1 document and plans their import.
func (migrationV2) PlanImport(doc *yaml.Node, ic ImportContext) (ImportPlan, error) {
	root := documentRoot(doc)
	if root == nil {
		return ImportPlan{}, fmt.Errorf("%w: the document root is not a mapping", ErrInvalidConfigDocument)
	}
	if err := refuseMarkers(doc, nil); err != nil {
		return ImportPlan{}, err
	}
	var plan ImportPlan
	taken := map[string]bool{}
	cleanup := map[string]bool{}

	for _, site := range credentialSites(root) {
		field := strings.Join(append(append([]string{}, site.path...), site.key), ".")
		valueIdx, fileIdx := mapIndex(site.parent, site.key), -1
		if site.fileKey != "" {
			fileIdx = mapIndex(site.parent, site.fileKey)
		}
		inline := ""
		if valueIdx >= 0 {
			if node := site.parent.Content[valueIdx+1]; node.Kind == yaml.ScalarNode && node.Tag != yamlNullTag {
				inline = node.Value
			}
		}
		fileRef := ""
		if fileIdx >= 0 {
			if node := site.parent.Content[fileIdx+1]; node.Kind == yaml.ScalarNode && node.Tag != yamlNullTag {
				fileRef = node.Value
			}
		}
		switch {
		case inline != "" && fileRef != "":
			return ImportPlan{}, fmt.Errorf("%w: %s sets both the inline value and its *_file companion; keep one", ErrSecretImport, field)
		case inline != "":
			name := assignName(field, site, taken)
			plan.Items = append(plan.Items, ImportItem{Name: name, Field: field, Value: inline})
			setReferenceNode(site.parent, site.key, TagSecret, name)
		case fileRef != "":
			abs := fileRef
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(ic.AgentDir, abs) // exactly how v1 loading resolved it
			}
			abs = filepath.Clean(abs)
			if within(ic.AgentDir, abs) {
				raw, err := os.ReadFile(abs)
				if err != nil {
					return ImportPlan{}, fmt.Errorf("%w: %s points at %s, which cannot be read; fix or remove it before migrating", ErrSecretImport, field, abs)
				}
				value := secretstore.NormalizeFileSecret(raw)
				if value == "" {
					return ImportPlan{}, fmt.Errorf("%w: %s points at %s, which is empty; fix or remove it before migrating", ErrSecretImport, field, abs)
				}
				name := assignName(field, site, taken)
				plan.Items = append(plan.Items, ImportItem{Name: name, Field: field, Value: value})
				setReferenceNode(site.parent, site.key, TagSecret, name)
				cleanup[abs] = true
			} else {
				setReferenceNode(site.parent, site.key, TagSecretFile, abs)
			}
		default:
			// Empty or null: the field is simply unset in v2.
			removeKey(site.parent, site.key)
		}
		if site.fileKey != "" {
			removeKey(site.parent, site.fileKey)
		}
	}

	// Agent identity: a retried migration reuses the existing store's id.
	id := ic.ExistingAgentID
	if id == "" {
		u, err := uuid.NewV7()
		if err != nil {
			return ImportPlan{}, fmt.Errorf("failed to generate the agent id: %w", err)
		}
		id = u.String()
	}
	plan.AgentID = id
	if mapIndex(root, "agent") < 0 {
		host, _ := os.Hostname()
		agent := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: yamlStrTag, Value: "id"},
			{Kind: yaml.ScalarNode, Tag: yamlStrTag, Value: id},
			{Kind: yaml.ScalarNode, Tag: yamlStrTag, Value: "name"},
			{Kind: yaml.ScalarNode, Tag: yamlStrTag, Style: yaml.DoubleQuotedStyle, Value: host},
		}}
		pos := mapIndex(root, versionKey)
		at := len(root.Content)
		if pos >= 0 {
			at = pos + 2
		}
		root.Content = append(root.Content[:at], append([]*yaml.Node{{Kind: yaml.ScalarNode, Tag: yamlStrTag, Value: "agent"}, agent}, root.Content[at:]...)...)
	}

	for p := range cleanup {
		plan.Cleanup = append(plan.Cleanup, p)
	}
	sort.Strings(plan.Cleanup)
	plan.Cleanup = append(plan.Cleanup, plaintextBackups(filepath.Join(ic.AgentDir, ConfigFileName))...)

	return plan, nil
}

func assignName(field string, site credSite, taken map[string]bool) string {
	base, ok := CanonicalSecretName(strings.Split(field, "."))
	if !ok || base == "" {
		base = sanitizeSecretName(strings.Join(site.path, "_") + "_" + site.key)
	}
	name := UniqueSecretName(base, func(n string) bool { return taken[n] })
	taken[name] = true

	return name
}

// setReferenceNode replaces the value of key with a tagged reference scalar.
func setReferenceNode(m *yaml.Node, key, tag, value string) {
	i := mapIndex(m, key)
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
	if i >= 0 {
		m.Content[i+1] = node

		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStrTag, Value: key}, node)
}
