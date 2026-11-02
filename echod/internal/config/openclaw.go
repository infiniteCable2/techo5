package config

// OpenClaw selects the native Gateway Talk backend. No speech provider credentials,
// models or acoustic settings belong on this connection configuration.
type OpenClaw struct {
	URL                string `json:"url,omitempty"`
	SessionKey         string `json:"session_key,omitempty"`
	StateDir           string `json:"state_dir,omitempty"`
	BootstrapTokenFile string `json:"bootstrap_token_file,omitempty"`
	BootstrapRole      string `json:"bootstrap_role,omitempty"`
	OutputWordTimings  bool   `json:"output_word_timings,omitempty"`
	// Explicitly request write authority for a separately approved personal agent.
	PersonalAgentAccess bool `json:"personal_agent_access,omitempty"`
}

// Enabled deliberately remains true for incomplete configuration: a selected but
// unavailable Gateway must never fall back to another assistant.
func (c OpenClaw) Enabled() bool { return c.URL != "" || c.SessionKey != "" }
