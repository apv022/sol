package sol

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Effects and approval policies of an action (API.md section 9).
const (
	EffectRead        = "read"
	EffectWrite       = "write"
	EffectDestructive = "destructive"

	ApprovalNever  = "never"
	ApprovalAgents = "agents"
	ApprovalAlways = "always"
)

// ActionSpec is one action entry of a manifest.
type ActionSpec struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Input       *Schema `json:"input,omitempty"`
	Effect      string  `json:"effect"`
	Approval    string  `json:"approval,omitempty"`
}

// Manifest is an app's app.json.
type Manifest struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	Exec        string   `json:"exec"`
	UI          string   `json:"ui,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Scopes      []string `json:"scopes"`
	RunsOn      []string `json:"runs_on,omitempty"`
	// Folders lists the subfolders of the synced folder a lens app may use on disk (API.md section 9).
	Folders []FolderAccess `json:"folders,omitempty"`
	// Reads lists other apps whose feeds this app may read.
	Reads   []string     `json:"reads,omitempty"`
	Actions []ActionSpec `json:"actions,omitempty"`
}

// FolderAccess is one entry of a manifest's folders list: a subfolder ("*" for all) and its mode
// ("read" or "write").
type FolderAccess struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

var knownScopes = map[string]bool{"feeds": true, "blobs": true, "people": true, "dial": true, "actions": true, "inbox": true}

// ParseManifest decodes and validates an app.json document.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("app.json: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadManifest reads and validates the app.json at path.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseManifest(data)
}

// Validate checks the manifest and fills defaults (runs_on "any", approval "never").
func (m *Manifest) Validate() error {
	if !ValidAppID(m.ID) {
		return fmt.Errorf("app.json: bad id %q", m.ID)
	}
	if m.Version == "" || m.Exec == "" {
		return fmt.Errorf("app.json: version and exec are required")
	}
	for _, s := range m.Scopes {
		if !knownScopes[s] {
			return fmt.Errorf("app.json: unknown scope %q", s)
		}
	}
	if len(m.RunsOn) == 0 {
		m.RunsOn = []string{"any"}
	}
	for _, r := range m.RunsOn {
		if r != "any" && r != "anchor" {
			return fmt.Errorf("app.json: bad runs_on %q", r)
		}
	}
	seen := map[string]bool{}
	for i := range m.Actions {
		a := &m.Actions[i]
		if !strings.HasPrefix(a.Name, m.ID+".") || len(a.Name) == len(m.ID)+1 {
			return fmt.Errorf("app.json: action %q must be named %s.<verb>", a.Name, m.ID)
		}
		if seen[a.Name] {
			return fmt.Errorf("app.json: duplicate action %q", a.Name)
		}
		seen[a.Name] = true
		switch a.Effect {
		case EffectRead, EffectWrite, EffectDestructive:
		default:
			return fmt.Errorf("app.json: action %q: bad effect %q", a.Name, a.Effect)
		}
		switch a.Approval {
		case "":
			a.Approval = ApprovalNever
		case ApprovalNever, ApprovalAgents, ApprovalAlways:
		default:
			return fmt.Errorf("app.json: action %q: bad approval %q", a.Name, a.Approval)
		}
	}
	return nil
}

// Action returns the declared action with the given name.
func (m *Manifest) Action(name string) (ActionSpec, bool) {
	for _, a := range m.Actions {
		if a.Name == name {
			return a, true
		}
	}
	return ActionSpec{}, false
}
