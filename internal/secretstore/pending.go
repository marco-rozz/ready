package secretstore

import (
	"sort"
)

// Reference is one configured sensitive field's reference, expressed without
// importing the config package. Exactly one of Name and File is set.
type Reference struct {
	// Field is the dotted config path, e.g. "llm.providers.main.api_key".
	Field string
	// Name is the Secret name of a `!secret` reference.
	Name string
	// File is the absolute path of a `!secret_file` reference.
	File string
}

// State is the derived status of a Secret name or external file.
type State string

const (
	// StatePending: referenced but absent, or present and non-conforming.
	StatePending State = "pending"
	// StateSet: present and conforming.
	StateSet State = "set"
	// StateUnused: present in the store but referenced by no field.
	StateUnused State = "unused"
	// StatePendingExternal: a `!secret_file` whose file is missing or empty.
	StatePendingExternal State = "pending_external"
)

// SecretStatus is the state of one Secret name. Value is never reported.
type SecretStatus struct {
	Name   string
	Fields []string
	State  State
	// Rule is the violated rule code when a present value is non-conforming.
	Rule string
}

// ExternalStatus is the state of one `!secret_file` reference.
type ExternalStatus struct {
	Field string
	Path  string
	State State
}

// Report is the daemon-derived view of every Secret and external reference.
type Report struct {
	Secrets  []SecretStatus
	External []ExternalStatus
}

// PendingNames lists the `!secret` names that need a value.
func (r Report) PendingNames() []string {
	var out []string
	for _, s := range r.Secrets {
		if s.State == StatePending {
			out = append(out, s.Name)
		}
	}

	return out
}

// PendingFields lists the config fields whose component cannot activate: every
// field of a pending name plus every pending external file.
func (r Report) PendingFields() []string {
	var out []string
	for _, s := range r.Secrets {
		if s.State == StatePending {
			out = append(out, s.Fields...)
		}
	}
	for _, e := range r.External {
		if e.State == StatePendingExternal {
			out = append(out, e.Field)
		}
	}
	sort.Strings(out)

	return out
}

// Derive computes the pending state. p may be nil (no store yet: every name is
// pending). readFile resolves a `!secret_file`; nil uses ReadFileSecret. Value
// rules are evaluated over the union of fields referencing each name.
func Derive(p *Payload, refs []Reference, readFile func(string) (string, bool, error)) Report {
	if readFile == nil {
		readFile = ReadFileSecret
	}
	byName := map[string][]string{}
	var report Report
	for _, r := range refs {
		if r.Name != "" {
			byName[r.Name] = append(byName[r.Name], r.Field)

			continue
		}
		st := StateSet
		if _, ok, err := readFile(r.File); err != nil || !ok {
			st = StatePendingExternal
		}
		report.External = append(report.External, ExternalStatus{Field: r.Field, Path: r.File, State: st})
	}
	if p != nil {
		for name := range p.Secrets {
			if _, ok := byName[name]; !ok {
				report.Secrets = append(report.Secrets, SecretStatus{Name: name, State: StateUnused})
			}
		}
	}
	for name, fields := range byName {
		sort.Strings(fields)
		st := SecretStatus{Name: name, Fields: fields, State: StatePending}
		if p != nil {
			if sec, ok := p.Secrets[name]; ok {
				if err := CheckValue(fields, sec.Value); err != nil {
					st.Rule = RuleCode(err)
				} else {
					st.State = StateSet
				}
			}
		}
		report.Secrets = append(report.Secrets, st)
	}
	sort.Slice(report.Secrets, func(i, j int) bool { return report.Secrets[i].Name < report.Secrets[j].Name })
	sort.Slice(report.External, func(i, j int) bool { return report.External[i].Field < report.External[j].Field })

	return report
}
