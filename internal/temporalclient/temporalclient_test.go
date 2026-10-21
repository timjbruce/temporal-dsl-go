package temporalclient

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const config = `
[profile.default]
address = "default.example:7233"
namespace = "default-ns"

[profile.dsl]
address = "dsl.example:7233"
namespace = "dsl-ns"

[profile.other]
address = "other.example:7233"
`

func useConfig(t *testing.T, toml string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "temporal.toml")
	require.NoError(t, os.WriteFile(path, []byte(toml), 0o600))
	t.Setenv("TEMPORAL_CONFIG_FILE", path)
	for _, k := range []string{"TEMPORAL_PROFILE", "TEMPORAL_ADDRESS", "TEMPORAL_NAMESPACE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestUsesDSLProfileByDefault(t *testing.T) {
	useConfig(t, config)
	opts, name, err := Options()
	require.NoError(t, err)
	require.Equal(t, "dsl", name)
	require.Equal(t, "dsl.example:7233", opts.HostPort)
	require.Equal(t, "dsl-ns", opts.Namespace)
}

func TestTemporalProfileOverrides(t *testing.T) {
	useConfig(t, config)
	t.Setenv("TEMPORAL_PROFILE", "other")
	opts, name, err := Options()
	require.NoError(t, err)
	require.Equal(t, "other", name)
	require.Equal(t, "other.example:7233", opts.HostPort)
}

func TestMissingExplicitProfileFails(t *testing.T) {
	useConfig(t, config)
	t.Setenv("TEMPORAL_PROFILE", "nope")
	_, _, err := Options()
	require.Error(t, err)
}

func TestMissingDSLProfileIgnoresDefaultProfile(t *testing.T) {
	useConfig(t, "[profile.default]\naddress = \"default.example:7233\"\n")
	opts, name, err := Options()
	require.NoError(t, err)
	require.Empty(t, name)
	require.Empty(t, opts.HostPort, "falls through to the SDK's localhost:7233")
}

func TestEnvVarsOverrideProfile(t *testing.T) {
	useConfig(t, config)
	t.Setenv("TEMPORAL_ADDRESS", "override.example:7233")
	opts, _, err := Options()
	require.NoError(t, err)
	require.Equal(t, "override.example:7233", opts.HostPort)
	require.Equal(t, "dsl-ns", opts.Namespace)
}
