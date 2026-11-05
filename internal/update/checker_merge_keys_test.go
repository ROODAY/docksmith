package update

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chis/docksmith/internal/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mergedImageCompose = `services:
  grampsweb: &grampsweb
    image: ghcr.io/gramps-project/grampsweb:2.3.0
  grampsweb_celery:
    <<: *grampsweb
    container_name: grampsweb_celery
`

// checkComposeMismatch must read an image inherited through a merge key,
// otherwise a drifted service sharing an anchor is never flagged.
func TestCheckComposeMismatch_FollowsMergeKeys(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "gramps.yaml")
	require.NoError(t, os.WriteFile(composePath, []byte(mergedImageCompose), 0644))

	container := docker.Container{
		ID:    "abc123",
		Name:  "grampsweb_celery",
		Image: "ghcr.io/gramps-project/grampsweb:2.4.0",
		Labels: map[string]string{
			"com.docker.compose.project.config_files": composePath,
		},
	}

	mismatch, expectedImage := (&Checker{}).checkComposeMismatch(container)
	assert.True(t, mismatch, "a running tag that differs from the inherited compose image is a mismatch")
	assert.Equal(t, "ghcr.io/gramps-project/grampsweb:2.3.0", expectedImage)
}

// checkEnvControlled must recognize an env-controlled image inherited through a
// merge key.
func TestCheckEnvControlled_FollowsMergeKeys(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "gramps.yaml")
	require.NoError(t, os.WriteFile(composePath, []byte(`services:
  grampsweb: &grampsweb
    image: ${GRAMPSWEB_IMAGE:-ghcr.io/gramps-project/grampsweb:2.3.0}
  grampsweb_celery:
    <<: *grampsweb
    container_name: grampsweb_celery
`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".env"),
		[]byte("GRAMPSWEB_IMAGE=ghcr.io/gramps-project/grampsweb:2.4.0\n"), 0644))

	container := docker.Container{
		Name: "grampsweb_celery",
		Labels: map[string]string{
			"com.docker.compose.project.config_files": composePath,
		},
	}

	var update ContainerUpdate
	(&Checker{}).checkEnvControlled(container, &update)

	assert.True(t, update.EnvControlled, "env-controlled image inherited via merge key should be detected")
	assert.Equal(t, "GRAMPSWEB_IMAGE", update.EnvVarName)
}
