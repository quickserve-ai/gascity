package main

import (
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// mailReadAPIClient returns (client, "") when the API path is available, or
// (nil, reason) when the caller should fall back. The client's mail read
// budget comes from the city's [mail] read_timeout, the same key the city's
// server derives its own mail read deadline from, so the server's typed
// store_slow answer arrives before the client gives up (pl-lzd).
func mailReadAPIClient(cityPath string) (*api.Client, string) {
	c := apiClient(cityPath)
	if c == nil {
		return nil, apiClientFallbackReason(cityPath)
	}
	c.SetMailReadTimeout(cityMailReadTimeout(cityPath))
	return c, ""
}

// cityMailReadTimeout returns the city's [mail] read_timeout from its
// city.toml, or config.DefaultMailReadTimeout when the file does not load.
// It reads city.toml alone, not the composed config, because it runs on every
// mail read including the prompt-submit hook; a city.toml that fails to load
// is reported by the command's own config load, not here.
func cityMailReadTimeout(cityPath string) time.Duration {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return config.DefaultMailReadTimeout
	}
	return cfg.Mail.EffectiveReadTimeout()
}
