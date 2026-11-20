package config

// Policy says what a repository's .curia/config.toml may do to one key
// (spec F28, Ruling 156). It can only tighten.
type Policy int

// The policies, one per kind of tightening.
const (
	PolicyNone      Policy = iota // nothing: the repository file cannot change this key
	PolicyLower                   // a number or duration: only a lower value
	PolicyRaise                   // a number or duration: only a higher value
	PolicyFalse                   // a bool: only false
	PolicyTrue                    // a bool: only true
	PolicyRepoRules               // a list of rules that apply to that repository only (RepoActivityRules); never merged into the global Config
)

// keyPolicy is the compiled-in per-key table. A new key is a Config field,
// its default in Default() and ONE line here; TestEveryKeyHasAPolicy fails
// when the line is missing. A key with PolicyNone (or no line) cannot be
// changed by a repository. Keys for features that do not exist yet are
// added with their feature; any key not named by a ruling stays PolicyNone.
var keyPolicy = map[string]Policy{
	"db_path":                PolicyNone,
	"log_path":               PolicyNone,
	"context_max":            PolicyNone,
	"default_context_max":    PolicyNone,
	"context_warn_pct":       PolicyNone,
	"context_crit_pct":       PolicyNone,
	"stale_hide_after":       PolicyNone,
	"assume_expiry":          PolicyNone,
	"notify_bell":            PolicyNone,
	"notify_macos":           PolicyNone,
	"routing.auto_send":      PolicyFalse,
	"routing.auto_threshold": PolicyRaise,
	"routing.aliases":        PolicyNone, // redirects text
	"key_dashboard":          PolicyNone,
	"key_decisions":          PolicyNone,
	"key_next":               PolicyNone,
	"key_message":            PolicyNone,
	"editor":                 PolicyNone, // an exec path (Ruling 107)
	"review.approve":         PolicyNone,
	"review.remote":          PolicyNone,
	"review.diff_tool":       PolicyNone, // an exec path
	"ntfy.url":               PolicyNone,
	"ntfy.quiet":             PolicyNone,
	"ntfy.devices":           PolicyNone,
	"serve.listen":           PolicyNone,
	"serve.public_url":       PolicyNone,
	"serve.web":              PolicyNone, // widens the network surface (F26)
	"serve.web_session":      PolicyNone,
	"restore.max_age":        PolicyNone,
	"usage.cheaper_model":    PolicyNone,
	"obsidian.dir":           PolicyNone,
	"linear.url":             PolicyNone,
	"linear.team":            PolicyNone,
	"costlens.analyzer":      PolicyNone, // an exec path
	"activity.idle_cap":      PolicyNone,
	"activity.task_types":    PolicyNone,
	"activity.rules":         PolicyRepoRules, // measurement only (F27, F28)
}
