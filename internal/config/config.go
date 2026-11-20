// Package config loads Curia's TOML configuration.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Config is Curia's runtime configuration.
type Config struct {
	DBPath            string         `toml:"db_path"`
	LogPath           string         `toml:"log_path"`
	ContextMax        map[string]int `toml:"context_max"`
	DefaultContextMax int            `toml:"default_context_max"`
	ContextWarnPct    int            `toml:"context_warn_pct"`
	ContextCritPct    int            `toml:"context_crit_pct"`
	StaleHideAfter    time.Duration  `toml:"stale_hide_after"`
	AssumeExpiry      time.Duration  `toml:"assume_expiry"`
	NotifyBell        bool           `toml:"notify_bell"`
	NotifyMacOS       bool           `toml:"notify_macos"`

	// Plan 3 additions.
	Routing Routing `toml:"routing"`
	// The tmux prefix-table keys curia install's snippet binds for the
	// dashboard popup, the decide popup, curia next and the message popup; ""
	// binds none.
	KeyDashboard string `toml:"key_dashboard"`
	KeyDecisions string `toml:"key_decisions"`
	KeyNext      string `toml:"key_next"`
	KeyMessage   string `toml:"key_message"`
	// Editor is the command `curia task edit` opens on a temp file, the
	// file's path appended as the last argument. It is split on single
	// spaces into an argument vector and run without a shell; there are no
	// quoting rules, so a path with spaces is not supported. The first word
	// is an absolute path or a bare program name looked up on PATH. Empty
	// (the default): no editor.
	Editor string `toml:"editor"`

	// Plan 4 additions.
	Review  Review  `toml:"review"`
	Ntfy    Ntfy    `toml:"ntfy"`
	Serve   Serve   `toml:"serve"`
	Restore Restore `toml:"restore"`

	// Plan 5 additions.
	Usage    Usage    `toml:"usage"`
	Obsidian Obsidian `toml:"obsidian"`
	Linear   Linear   `toml:"linear"`
	CostLens CostLens `toml:"costlens"`
}

// maxEditor bounds the editor option, in bytes.
const maxEditor = 1024

// EditorArgs is Editor as an argument vector, or nil when it is empty.
func (c Config) EditorArgs() []string {
	if c.Editor == "" {
		return nil
	}
	return strings.Split(c.Editor, " ")
}

// validateEditor checks the editor option: at most maxEditor bytes, no
// control or other unprintable character, every word between single spaces
// non-empty, and a first word that is an absolute path or a bare name.
func validateEditor(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > maxEditor {
		return fmt.Errorf("editor must be at most %d bytes, got %d", maxEditor, len(v))
	}
	for _, r := range v {
		if r != ' ' && !unicode.IsPrint(r) {
			return fmt.Errorf("editor must not hold control or unprintable characters, found %U", r)
		}
	}
	words := strings.Split(v, " ")
	for _, w := range words {
		if w == "" {
			return errors.New("editor must be words separated by single spaces, with none at either end (a path with spaces is not supported)")
		}
	}
	if p := words[0]; strings.Contains(p, "/") && !filepath.IsAbs(p) {
		return fmt.Errorf("editor's program %q must be an absolute path or a bare name looked up on PATH", p)
	}
	return nil
}

// Default returns the configuration used when no file overrides it.
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		DBPath:            filepath.Join(home, ".local/share/curia/curia.db"),
		LogPath:           filepath.Join(home, ".local/state/curia/curia.log"),
		ContextMax:        map[string]int{},
		DefaultContextMax: 200000,
		ContextWarnPct:    60,
		ContextCritPct:    85,
		StaleHideAfter:    10 * time.Minute,
		AssumeExpiry:      24 * time.Hour,

		// Plan 3 additions.
		Routing:      defaultRouting(),
		KeyDashboard: "a",
		KeyDecisions: "A",
		KeyNext:      "N",
		KeyMessage:   "m",

		// Plan 4 additions.
		Review:  Review{Approve: "merge", Remote: "origin", DiffTool: "git"},
		Ntfy:    Ntfy{Devices: map[string]string{}},
		Restore: Restore{MaxAge: 24 * time.Hour},

		// Plan 5 additions.
		Usage:  Usage{CheaperModel: "sonnet"},
		Linear: Linear{URL: DefaultLinearURL},
	}
}

// Load resolves the configuration (see Resolve) from the user file at path
// and the repository file of the current directory's repository. A missing
// file yields the defaults. A leading "~" in the paths is expanded, and the
// CURIA_DB environment variable, when set, overrides DBPath last.
func Load(path string) (Config, error) {
	wd, _ := os.Getwd()
	c, _, err := Resolve(Options{UserPath: path, RepoRoot: FindRepoRoot(wd)})
	return c, err
}

// validate checks every section, each error naming its key, and normalizes
// the paths and URLs.
func (c *Config) validate() error {
	if c.AssumeExpiry <= 0 {
		return fmt.Errorf("assume_expiry must be positive, got %v", c.AssumeExpiry)
	}
	if err := c.Routing.validate(); err != nil {
		return err
	}
	if err := validateEditor(c.Editor); err != nil {
		return err
	}
	if err := c.validatePlan4(); err != nil {
		return err
	}
	return c.validatePlan5()
}

func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// ---- Plan 4: review, ntfy, serve and restore ----

// Review configures how a finished task is reviewed and approved.
type Review struct {
	Approve  string `toml:"approve"`   // "merge" (default) or "pr"
	Remote   string `toml:"remote"`    // default "origin"
	DiffTool string `toml:"diff_tool"` // "git" (default), "delta", "lazygit", "nvim"
}

// Ntfy configures phone pushes. Empty URL or no devices: no pushes.
type Ntfy struct {
	URL     string            `toml:"url"`
	Quiet   string            `toml:"quiet"`
	Devices map[string]string `toml:"devices"` // device name -> topic; never nil
}

// Serve configures `curia serve`.
type Serve struct {
	Listen    string `toml:"listen"`
	PublicURL string `toml:"public_url"`
}

// Restore configures `curia restore`.
type Restore struct {
	MaxAge time.Duration `toml:"max_age"` // default 24h
}

const (
	maxURL     = 200
	maxDevices = 8
)

var (
	remoteName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	deviceName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	topicName  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

	tailscale4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscale6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// Enabled reports whether pushes are on: a URL and at least one device.
func (n Ntfy) Enabled() bool { return n.URL != "" && len(n.Devices) > 0 }

// DeviceNames returns the device names, sorted.
func (n Ntfy) DeviceNames() []string {
	names := make([]string, 0, len(n.Devices))
	for d := range n.Devices {
		names = append(names, d)
	}
	sort.Strings(names)
	return names
}

// ParseQuiet reads "HH:MM-HH:MM" as minutes after midnight. ok is false for
// "" and for anything else, including two equal times.
func ParseQuiet(s string) (start, end int, ok bool) {
	if len(s) != 11 || s[5] != '-' {
		return 0, 0, false
	}
	start, ok = clockMinutes(s[:5])
	if !ok {
		return 0, 0, false
	}
	end, ok = clockMinutes(s[6:])
	if !ok || start == end {
		return 0, 0, false
	}
	return start, end, true
}

// clockMinutes reads exactly "HH:MM" as minutes after midnight.
func clockMinutes(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// TailscaleAddr reports whether a is in 100.64.0.0/10 or fd7a:115c:a1e0::/48.
func TailscaleAddr(a netip.Addr) bool {
	a = a.Unmap()
	return tailscale4.Contains(a) || tailscale6.Contains(a)
}

// validatePlan4 checks the Plan 4 sections, each error naming its key, and
// drops the trailing "/" of both URLs.
func (c *Config) validatePlan4() error {
	if err := c.Review.validate(); err != nil {
		return err
	}
	if c.Ntfy.Devices == nil {
		c.Ntfy.Devices = map[string]string{}
	}
	if err := c.Ntfy.validate(); err != nil {
		return err
	}
	if err := c.Serve.validate(); err != nil {
		return err
	}
	if c.Restore.MaxAge <= 0 {
		return fmt.Errorf("restore.max_age must be above zero, got %v", c.Restore.MaxAge)
	}
	c.Ntfy.URL = strings.TrimRight(c.Ntfy.URL, "/")
	c.Serve.PublicURL = strings.TrimRight(c.Serve.PublicURL, "/")
	return nil
}

func (r Review) validate() error {
	if r.Approve != "merge" && r.Approve != "pr" {
		return fmt.Errorf("review.approve must be merge or pr, got %q", r.Approve)
	}
	switch r.DiffTool {
	case "git", "delta", "lazygit", "nvim":
	default:
		return fmt.Errorf("review.diff_tool must be git, delta, lazygit or nvim, got %q", r.DiffTool)
	}
	if !remoteName.MatchString(r.Remote) || strings.HasPrefix(r.Remote, "-") {
		return fmt.Errorf("review.remote must match %s and not start with -, got %q", remoteName, r.Remote)
	}
	return nil
}

func (n Ntfy) validate() error {
	if err := validateURL("ntfy.url", n.URL); err != nil {
		return err
	}
	// Curia publishes JSON to the base URL; ntfy stores a JSON body POSTed
	// to /<topic> as plain text (probe N1), so no path is allowed.
	if u, err := url.Parse(n.URL); err == nil && u.Path != "" && u.Path != "/" {
		return fmt.Errorf("ntfy.url must be the server's base URL with no path (the topic goes in [ntfy.devices]), got %q", n.URL)
	}
	if n.Quiet != "" {
		if _, _, ok := ParseQuiet(n.Quiet); !ok {
			return fmt.Errorf("ntfy.quiet must be HH:MM-HH:MM with two different times, got %q", n.Quiet)
		}
	}
	if len(n.Devices) > maxDevices {
		return fmt.Errorf("ntfy.devices holds %d devices, at most %d are allowed", len(n.Devices), maxDevices)
	}
	for _, d := range n.DeviceNames() {
		if !deviceName.MatchString(d) {
			return fmt.Errorf("ntfy.devices.%s: name must match %s", d, deviceName)
		}
		if !topicName.MatchString(n.Devices[d]) {
			return fmt.Errorf("ntfy.devices.%s: topic must match %s", d, topicName)
		}
	}
	return nil
}

func (s Serve) validate() error {
	if err := validateURL("serve.public_url", s.PublicURL); err != nil {
		return err
	}
	if s.Listen == "" {
		return nil
	}
	ap, err := netip.ParseAddrPort(s.Listen)
	if err != nil {
		return fmt.Errorf("serve.listen must be ip:port with an IP literal, got %q", s.Listen)
	}
	if a := ap.Addr(); !a.IsLoopback() && !TailscaleAddr(a) {
		return fmt.Errorf("serve.listen must be a loopback or Tailscale address, got %q", s.Listen)
	}
	if ap.Port() == 0 {
		return fmt.Errorf("serve.listen port must be 1 to 65535, got %q", s.Listen)
	}
	return nil
}

// validateURL checks an ntfy or public URL: empty is allowed; otherwise at
// most maxURL bytes, http or https, a host, no user info, query or fragment,
// and plain http only to a loopback, Tailscale or .ts.net host.
func validateURL(key, raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > maxURL {
		return fmt.Errorf("%s must be at most %d bytes, got %d", key, maxURL, len(raw))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s does not parse: %w", key, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must be an http or https URL", key)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%s has no host", key)
	}
	if u.User != nil || strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("%s must not hold user info, a query or a fragment", key)
	}
	if u.Scheme == "http" && !privateHost(host) {
		return fmt.Errorf("%s: plain http is allowed only to localhost, a loopback or Tailscale address, or a .ts.net name", key)
	}
	return nil
}

func privateHost(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".ts.net") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && (a.IsLoopback() || TailscaleAddr(a))
}

// ---- Plan 5: usage, obsidian, linear and costlens ----

// DefaultLinearURL is the Linear GraphQL endpoint.
const DefaultLinearURL = "https://api.linear.app/graphql"

// Usage configures limit forecasting.
type Usage struct {
	CheaperModel string `toml:"cheaper_model"` // default "sonnet"; "" turns the spawn default off
}

// Obsidian configures the decision-note export.
type Obsidian struct {
	Dir string `toml:"dir"` // "~" expanded; "" means no export
}

// Linear configures `curia task import --linear`. The API key is never a
// config option: it comes only from CURIA_LINEAR_API_KEY.
type Linear struct {
	URL  string `toml:"url"` // default DefaultLinearURL
	Team string `toml:"team"`
}

// CostLens configures the cost-lens analyzer.
type CostLens struct {
	Analyzer string `toml:"analyzer"` // "~" expanded
}

// maxPath bounds the Plan 5 path options, in bytes.
const maxPath = 1024

var (
	cheaperModel = regexp.MustCompile(`^[A-Za-z0-9._\[\]-]{1,64}$`)
	linearTeam   = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)
)

// validatePlan5 checks the Plan 5 sections, each error naming its key, and
// expands "~" in the two path options.
func (c *Config) validatePlan5() error {
	if c.Usage.CheaperModel != "" && !cheaperModel.MatchString(c.Usage.CheaperModel) {
		return fmt.Errorf("usage.cheaper_model must match %s or be empty, got %q", cheaperModel, c.Usage.CheaperModel)
	}
	dir, err := absPathOption("obsidian.dir", c.Obsidian.Dir)
	if err != nil {
		return err
	}
	c.Obsidian.Dir = dir
	if err = c.Linear.validate(); err != nil {
		return err
	}
	an, err := absPathOption("costlens.analyzer", c.CostLens.Analyzer)
	if err != nil {
		return err
	}
	if an != "" && !strings.HasSuffix(an, ".js") && !strings.HasSuffix(an, ".ts") {
		return fmt.Errorf("costlens.analyzer must end in .js or .ts, got %q", c.CostLens.Analyzer)
	}
	c.CostLens.Analyzer = an
	return nil
}

// absPathOption expands "~" in v and requires an absolute path of at most
// maxPath bytes with no rune below 0x20. Empty is allowed.
func absPathOption(key, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	p := expandHome(v)
	if len(p) > maxPath {
		return "", fmt.Errorf("%s must be at most %d bytes, got %d", key, maxPath, len(p))
	}
	for _, r := range p {
		if r < 0x20 {
			return "", fmt.Errorf("%s must not hold control characters, found %U", key, r)
		}
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s must be an absolute path (after ~ is expanded), got %q", key, v)
	}
	return p, nil
}

func (l Linear) validate() error {
	if l.Team != "" && !linearTeam.MatchString(l.Team) {
		return fmt.Errorf("linear.team must match %s or be empty, got %q", linearTeam, l.Team)
	}
	raw := l.URL
	if len(raw) > maxURL {
		return fmt.Errorf("linear.url must be at most %d bytes, got %d", maxURL, len(raw))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("linear.url does not parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("linear.url must be an http or https URL")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("linear.url has no host")
	}
	if u.User != nil || strings.ContainsAny(raw, "?#") {
		return errors.New("linear.url must not hold user info, a query or a fragment")
	}
	if u.Scheme == "http" && !loopbackHost(host) {
		return errors.New("linear.url: plain http is allowed only to localhost or a loopback address")
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}
