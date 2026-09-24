package settings

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
)

const (
	defaultConfigDir = "config"
	configDirEnv     = "SOLSTEIN_CONFIG_DIR"
)

// Startup holds process-level options that aren't part of config.json:
// they're needed before config.json can be found, or they request a one-off
// action rather than a setting.
type Startup struct {
	ConfigDir   string
	ShowVersion bool
}

// setting is one config.json field that can be changed by a flag or an
// environment variable. Declaring both names in one table keeps them in sync,
// and means entrypoint.sh doesn't need to map every variable to a flag.
type setting struct {
	flag  string
	env   string
	usage string
	apply func(cfg *Config, value string) error
}

var settings = []setting{
	{
		flag:  "port",
		env:   "SOLSTEIN_PORT",
		usage: "Port Solstein listens on.",
		apply: func(cfg *Config, value string) error {
			port, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("not a number: %q", value)
			}
			cfg.Port = port
			return nil
		},
	},
	{
		flag:  "externalurl",
		env:   "SOLSTEIN_EXTERNAL_URL",
		usage: "URL Audiobookshelf uses to reach Solstein, e.g. http://solstein:8080.",
		apply: func(cfg *Config, value string) error {
			cfg.ExternalURL = value
			return nil
		},
	},
	{
		flag:  "loglevel",
		env:   "SOLSTEIN_LOG_LEVEL",
		usage: "Log level: trace, debug, info, warn, error.",
		apply: func(cfg *Config, value string) error {
			cfg.LogLevel = value
			return nil
		},
	},
	{
		flag:  "timezone",
		env:   "SOLSTEIN_TIMEZONE",
		usage: "IANA time zone, e.g. Europe/Oslo. Empty uses the system time zone (TZ).",
		apply: func(cfg *Config, value string) error {
			cfg.Timezone = value
			return nil
		},
	},
}

// Resolve parses command-line arguments, loads config.json from the data
// directory, applies flag and environment overrides on top, validates the
// result and saves it back to config.json. getenv is os.Getenv outside tests.
// Nothing is saved if validation fails, so a bad flag or environment variable
// can't corrupt the file. If args ask for help or the version, the returned
// Config is zero and config.json is not touched.
func Resolve(args []string, getenv func(string) string, output io.Writer) (Config, Startup, error) {
	fs := flag.NewFlagSet("solstein", flag.ContinueOnError)
	fs.SetOutput(output)

	// Flag values are collected rather than applied straight away, because
	// config.json can only be loaded once -configdir is known.
	provided := map[string]string{}
	for _, s := range settings {
		fs.Func(s.flag, s.usage+" Env: "+s.env+".", func(value string) error {
			provided[s.flag] = value
			return nil
		})
	}

	var startup Startup
	configDirDefault := getenv(configDirEnv)
	if configDirDefault == "" {
		configDirDefault = defaultConfigDir
	}
	fs.StringVar(&startup.ConfigDir, "configdir", configDirDefault, "Directory for config.json, logs and cached episodes. Env: "+configDirEnv+".")
	fs.BoolVar(&startup.ShowVersion, "version", false, "Print the version and exit.")

	if err := fs.Parse(args); err != nil {
		return Config{}, startup, err
	}
	if fs.NArg() > 0 {
		return Config{}, startup, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if startup.ShowVersion {
		return Config{}, startup, nil
	}

	if err := os.MkdirAll(startup.ConfigDir, 0o750); err != nil {
		return Config{}, startup, fmt.Errorf("create config directory %s: %w", startup.ConfigDir, err)
	}

	cfg, err := Load(startup.ConfigDir)
	if err != nil {
		return Config{}, startup, err
	}

	for _, s := range settings {
		if value, ok := provided[s.flag]; ok {
			if err := s.apply(&cfg, value); err != nil {
				return Config{}, startup, fmt.Errorf("flag -%s: %w", s.flag, err)
			}
		} else if value := getenv(s.env); value != "" {
			if err := s.apply(&cfg, value); err != nil {
				return Config{}, startup, fmt.Errorf("environment variable %s: %w", s.env, err)
			}
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, startup, err
	}

	if err := Save(startup.ConfigDir, cfg); err != nil {
		return Config{}, startup, err
	}

	return cfg, startup, nil
}
