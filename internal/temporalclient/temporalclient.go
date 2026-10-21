// Package temporalclient is the one place the worker and the web server get
// their Temporal connection from. It reads the same temporal.toml and
// TEMPORAL_* variables as the temporal CLI, but defaults to this project's
// own profile rather than the user's "default" one.
package temporalclient

import (
	"fmt"
	"os"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"
)

// Profile is the temporal.toml profile this project uses unless
// TEMPORAL_PROFILE names another.
const Profile = "dsl"

// Options resolves the client options and reports which profile supplied
// them; the name is empty when no profile applied and only TEMPORAL_*
// variables (or the SDK's localhost:7233 / "default" namespace) were used.
//
// An explicit TEMPORAL_PROFILE must exist, as it must for the CLI. A missing
// [profile.dsl] is not an error: it falls through to the local dev server
// rather than to [profile.default], which is often someone's own Cloud
// namespace.
func Options() (client.Options, string, error) {
	conf, err := envconfig.LoadClientConfig(envconfig.LoadClientConfigOptions{})
	if err != nil {
		return client.Options{}, "", err
	}

	name, explicit := os.LookupEnv("TEMPORAL_PROFILE")
	explicit = explicit && name != ""
	if !explicit {
		name = Profile
	}

	var prof envconfig.ClientConfigProfile
	if p := conf.Profiles[name]; p != nil {
		prof = *p
	} else if explicit {
		return client.Options{}, "", fmt.Errorf("profile %q not found in Temporal config", name)
	} else {
		name = ""
	}

	if err := prof.ApplyEnvVars(envconfig.EnvLookupOS); err != nil {
		return client.Options{}, "", err
	}
	opts, err := prof.ToClientOptions(envconfig.ToClientOptionsRequest{})
	return opts, name, err
}
