package oracle

import (
	"fmt"
	mrand "math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Datadir holds paths for an oracle test's datadir. Replaces the per-
// client `oracleDatadir` struct that was duplicated 4× across e2e tests.
//
// In direct (non-DinD) mode HostPath == ContainerDatadir == VolMount's
// host side. In DinD-with-named-volume mode (the CI setup), HostPath is
// the test container's bind mount of the named volume + per-test
// subdir; ContainerDatadir is the spawned client container's view of
// the same per-test subdir.
type Datadir struct {
	HostPath         string // path on the test container's filesystem
	VolMount         string // -v argument: "name:/data" (DinD) or "host-path:/data" (direct)
	ContainerDatadir string // path the spawned client container sees as datadir
}

// AcquireDatadir returns a Datadir for the calling test plus a cleanup
// function. Honors two env vars (per-client prefixed) that `make
// test-{client}-suite` sets when running inside Docker (DinD via socket
// mount): {ENV_BASE}_ORACLE_DATADIR and {ENV_BASE}_ORACLE_VOL.
//
// envBase is the per-client prefix — "BESU" / "NETH" / "RETH" / "GETH".
// The volume-name env var is derived as envBase + "_ORACLE_VOL".
//
// Falls back to t.TempDir() for direct host runs.
//
// Replaces the 4 per-client `acquireOracleDatadir` functions that
// previously duplicated this logic.
func AcquireDatadir(t *testing.T, envBase string) (Datadir, func()) {
	t.Helper()
	baseDir := os.Getenv(envBase + "_ORACLE_DATADIR")
	if baseDir == "" {
		datadir := t.TempDir()
		return Datadir{
			HostPath:         datadir,
			VolMount:         datadir + ":/data",
			ContainerDatadir: "/data",
		}, func() {}
	}
	subName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	hostPath := baseDir + "/" + subName
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		t.Fatalf("AcquireDatadir: mkdir %s: %v", hostPath, err)
	}
	t.Logf("using datadir=%s", hostPath)

	vol := os.Getenv(envBase + "_ORACLE_VOL")
	volMount := hostPath + ":/data"
	containerDatadir := "/data"
	if vol != "" {
		volMount = vol + ":/data"
		containerDatadir = "/data/" + subName
	}
	return Datadir{
		HostPath:         hostPath,
		VolMount:         volMount,
		ContainerDatadir: containerDatadir,
	}, func() {}
}

// InspectContainerIP returns the bridge-network IP of a running
// container. In DinD mode (test runs inside a container with
// /var/run/docker.sock mounted), the spawned client container's
// host-port mapping lives on the Docker host VM, not on the test
// container's network — but both containers share the Docker daemon's
// default bridge network, so they reach each other by bridge IP.
//
// Replaces the 3 per-client `inspectContainerIP` copies that were
// duplicated in besu/neth/reth e2e tests.
func InspectContainerIP(containerName string) (string, error) {
	out, err := exec.Command("docker", "inspect",
		"--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
		containerName,
	).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", containerName, err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("container %s has no bridge IP (not yet started?)", containerName)
	}
	return ip, nil
}

// RandSuffix returns a random lower-hex suffix of length n for unique
// container names. Uses time-seeded math/rand because uniqueness, not
// crypto strength, is what matters here.
func RandSuffix(n int) string {
	const chars = "abcdef0123456789"
	r := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return string(b)
}

// DockerPlatformArgs returns ["--platform", $envVar] when the env var
// is set, otherwise nil. Each client's e2e test passes its own env-var
// name (BESU_DOCKER_PLATFORM / NETH_DOCKER_PLATFORM / etc.) — the
// override unblocks arm64 contributors when the pinned image is
// amd64-only (see nerolation/state-actor#43).
func DockerPlatformArgs(envVar string) []string {
	if v := os.Getenv(envVar); v != "" {
		return []string{"--platform", v}
	}
	return nil
}
