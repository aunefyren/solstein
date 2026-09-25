package settings

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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
	// boolean flags can be given bare (-flag) as well as -flag=true/false.
	boolean bool
	apply   func(cfg *Config, value string) error
}

// boolSetting builds the apply function for a boolean setting.
func boolSetting(target func(cfg *Config) *bool) func(cfg *Config, value string) error {
	return func(cfg *Config, value string) error {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("not true or false: %q", value)
		}
		*target(cfg) = parsed
		return nil
	}
}

// intSetting builds the apply function for a whole-number setting.
func intSetting(target func(cfg *Config) *int) func(cfg *Config, value string) error {
	return func(cfg *Config, value string) error {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("not a number: %q", value)
		}
		*target(cfg) = parsed
		return nil
	}
}

// listSetting builds the apply function for a comma-separated list setting.
func listSetting(target func(cfg *Config) *[]string) func(cfg *Config, value string) error {
	return func(cfg *Config, value string) error {
		list := []string{}
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				list = append(list, part)
			}
		}
		*target(cfg) = list
		return nil
	}
}

var settings = []setting{
	{
		flag:  "port",
		env:   "SOLSTEIN_PORT",
		usage: "Port Solstein listens on.",
		apply: intSetting(func(cfg *Config) *int { return &cfg.Port }),
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
		flag:    "allowprivatedestinations",
		env:     "SOLSTEIN_ALLOW_PRIVATE_DESTINATIONS",
		usage:   "Allow fetching feeds and episodes from private and loopback addresses.",
		boolean: true,
		apply:   boolSetting(func(cfg *Config) *bool { return &cfg.AllowPrivateDestinations }),
	},
	{
		flag:    "disableauth",
		env:     "SOLSTEIN_DISABLE_AUTH",
		usage:   "Turn off the subscribe token and URL signatures. Only for private networks.",
		boolean: true,
		apply:   boolSetting(func(cfg *Config) *bool { return &cfg.DisableAuth }),
	},
	{
		flag:  "authtoken",
		env:   "SOLSTEIN_AUTH_TOKEN",
		usage: "Subscribe token; generated on first run if not set.",
		apply: func(cfg *Config, value string) error {
			cfg.AuthToken = value
			return nil
		},
	},
	{
		flag:  "allowedclientnetworks",
		env:   "SOLSTEIN_ALLOWED_CLIENT_NETWORKS",
		usage: "Comma-separated IPs/CIDRs allowed to use Solstein; empty allows any.",
		apply: listSetting(func(cfg *Config) *[]string { return &cfg.AllowedClientNetworks }),
	},
	{
		flag:  "trustedproxies",
		env:   "SOLSTEIN_TRUSTED_PROXIES",
		usage: "Comma-separated IPs/CIDRs of reverse proxies whose X-Forwarded-For is trusted.",
		apply: listSetting(func(cfg *Config) *[]string { return &cfg.TrustedProxies }),
	},
	{
		flag:  "allowedsourcehosts",
		env:   "SOLSTEIN_ALLOWED_SOURCE_HOSTS",
		usage: "Comma-separated hosts feeds may be subscribed from (subdomains included); empty allows any.",
		apply: listSetting(func(cfg *Config) *[]string { return &cfg.AllowedSourceHosts }),
	},
	{
		flag:  "defaultexit",
		env:   "SOLSTEIN_DEFAULT_EXIT",
		usage: "Exit for feeds that don't name one; empty means direct.",
		apply: func(cfg *Config, value string) error {
			cfg.DefaultExit = value
			return nil
		},
	},
	{
		flag:    "disabledirect",
		env:     "SOLSTEIN_DISABLE_DIRECT",
		usage:   "Never use the host's own connection; needs a default exit.",
		boolean: true,
		apply:   boolSetting(func(cfg *Config) *bool { return &cfg.DisableDirect }),
	},
	{
		flag:  "homecountry",
		env:   "SOLSTEIN_HOME_COUNTRY",
		usage: "Country this host's own connection comes out in (e.g. NO), for region diff's same-country checks; empty means unknown.",
		apply: func(cfg *Config, value string) error {
			cfg.HomeCountry = value
			return nil
		},
	},
	{
		flag:  "deliverymode",
		env:   "SOLSTEIN_DELIVERY_MODE",
		usage: "Default episode delivery: cache, stream or original.",
		apply: func(cfg *Config, value string) error {
			cfg.DeliveryMode = value
			return nil
		},
	},
	{
		flag:  "pollinterval",
		env:   "SOLSTEIN_POLL_INTERVAL",
		usage: "Minutes between feed polls.",
		apply: intSetting(func(cfg *Config) *int { return &cfg.PollIntervalMinutes }),
	},
	{
		flag:  "cacheretention",
		env:   "SOLSTEIN_CACHE_RETENTION",
		usage: "Days cached episodes are kept.",
		apply: intSetting(func(cfg *Config) *int { return &cfg.CacheRetentionDays }),
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
		collect := func(value string) error {
			provided[s.flag] = value
			return nil
		}
		if s.boolean {
			fs.BoolFunc(s.flag, s.usage+" Env: "+s.env+".", collect)
		} else {
			fs.Func(s.flag, s.usage+" Env: "+s.env+".", collect)
		}
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
