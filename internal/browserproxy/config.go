package browserproxy

import (
	"os"
	"path/filepath"
	"time"

	"ds2api/internal/config"
)

const (
	DefaultTimeoutSeconds = 180
	DefaultPollIntervalMs = 50
	DefaultUserDataDir    = "./browser_profile"
)

type Config struct {
	Enabled        bool
	Headless       bool
	UserDataDir    string
	TimeoutSeconds int
	PollIntervalMs int
}

func NewConfig(storeCfg config.BrowserProxyConfig) Config {
	return Config{
		Enabled:        storeCfg.Enabled != nil && *storeCfg.Enabled,
		Headless:       storeCfg.Headless == nil || *storeCfg.Headless,
		UserDataDir:    resolveUserDataDir(storeCfg.UserDataDir),
		TimeoutSeconds: resolveTimeoutSeconds(storeCfg.TimeoutSeconds),
		PollIntervalMs: resolvePollIntervalMs(storeCfg.PollIntervalMs),
	}
}

func (c Config) Timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return time.Duration(DefaultTimeoutSeconds) * time.Second
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

func (c Config) PollInterval() time.Duration {
	if c.PollIntervalMs <= 0 {
		return time.Duration(DefaultPollIntervalMs) * time.Millisecond
	}
	return time.Duration(c.PollIntervalMs) * time.Millisecond
}

func resolveUserDataDir(dir string) string {
	if dir == "" {
		dir = DefaultUserDataDir
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	if _, statErr := os.Stat(absDir); os.IsNotExist(statErr) {
		os.MkdirAll(absDir, 0755)
	}
	return absDir
}

func resolveTimeoutSeconds(seconds int) int {
	if seconds <= 0 {
		return DefaultTimeoutSeconds
	}
	return seconds
}

func resolvePollIntervalMs(ms int) int {
	if ms <= 0 {
		return DefaultPollIntervalMs
	}
	return ms
}
