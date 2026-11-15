package config

import (
	"fmt"
	"regexp"
	"sort"
)

// Routing configures how `curia say` routes messages.
type Routing struct {
	AutoSend      bool              `toml:"auto_send"`
	AutoThreshold float64           `toml:"auto_threshold"`
	Aliases       map[string]string `toml:"aliases"`
}

var aliasName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

const maxAliasValue = 200

func defaultRouting() Routing {
	return Routing{AutoThreshold: 0.9, Aliases: map[string]string{}}
}

// validate checks the routing section, naming the offending key.
func (r *Routing) validate() error {
	if !(r.AutoThreshold > 0 && r.AutoThreshold <= 1) {
		return fmt.Errorf("routing.auto_threshold must be in (0, 1], got %v", r.AutoThreshold)
	}
	if r.Aliases == nil {
		r.Aliases = map[string]string{}
	}
	names := make([]string, 0, len(r.Aliases))
	for n := range r.Aliases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !aliasName.MatchString(n) {
			return fmt.Errorf("routing.aliases.%s: name must match %s", n, aliasName)
		}
		if v := r.Aliases[n]; v == "" || len(v) > maxAliasValue {
			return fmt.Errorf("routing.aliases.%s: value must be 1 to %d bytes, got %d", n, maxAliasValue, len(v))
		}
	}
	return nil
}
