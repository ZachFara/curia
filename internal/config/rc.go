package config

// RC configures F21 Remote Control. Remote Control exposes a session outside
// the machine, so no repository file can set it (policy.go).
type RC struct {
	// Launch adds --remote-control to the claude command of sessions Curia
	// launches itself (default false). UNVERIFIED: see cmd/curia/rc.go.
	Launch bool `toml:"launch"`
}
