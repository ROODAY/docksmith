package update

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chis/docksmith/internal/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func composeLabels(file, service string) map[string]string {
	return map[string]string{
		"com.docker.compose.project.config_files": file,
		"com.docker.compose.service":              service,
	}
}

func containerNamesOf(containers []*docker.Container) []string {
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		names = append(names, c.Name)
	}
	return names
}

// Selecting one service whose image comes from a shared anchor must pull the
// other inheritors into the batch with the same target version.
func TestExpandLockstepContainers_PullsInAnchorSharers(t *testing.T) {
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "gramps.yaml")
	require.NoError(t, os.WriteFile(composeFile, []byte(`services:
  grampsweb: &grampsweb
    image: ghcr.io/gramps-project/grampsweb:2.3.0
    container_name: grampsweb
  grampsweb_celery:
    <<: *grampsweb
    container_name: grampsweb_celery
  grampsweb_redis:
    image: redis:7
    container_name: grampsweb_redis
`), 0644))

	all := []*docker.Container{
		{Name: "grampsweb", Image: "ghcr.io/gramps-project/grampsweb:2.3.0", Labels: composeLabels(composeFile, "grampsweb")},
		{Name: "grampsweb_celery", Image: "ghcr.io/gramps-project/grampsweb:2.3.0", Labels: composeLabels(composeFile, "grampsweb_celery")},
		{Name: "grampsweb_redis", Image: "redis:7", Labels: composeLabels(composeFile, "grampsweb_redis")},
	}
	batch := []*docker.Container{all[1]}
	targets := map[string]string{"grampsweb_celery": "2.4.0"}

	orch := &UpdateOrchestrator{}
	expanded, expandedTargets := orch.expandLockstepContainers(context.Background(), batch, all, targets)

	assert.ElementsMatch(t, []string{"grampsweb", "grampsweb_celery"}, containerNamesOf(expanded),
		"grampsweb inherits the same anchor and must be updated in lockstep")
	assert.Equal(t, "2.4.0", expandedTargets["grampsweb"])
	assert.Equal(t, "2.4.0", expandedTargets["grampsweb_celery"])
}

// Services that merely use the same literal image string have independent image
// nodes and must not be dragged into each other's batches.
func TestExpandLockstepContainers_LeavesLiteralImagesAlone(t *testing.T) {
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "docker-compose.yaml")
	require.NoError(t, os.WriteFile(composeFile, []byte(`services:
  a:
    image: nginx:1.20
    container_name: a
  b:
    image: nginx:1.20
    container_name: b
`), 0644))

	all := []*docker.Container{
		{Name: "a", Image: "nginx:1.20", Labels: composeLabels(composeFile, "a")},
		{Name: "b", Image: "nginx:1.20", Labels: composeLabels(composeFile, "b")},
	}
	batch := []*docker.Container{all[0]}

	orch := &UpdateOrchestrator{}
	expanded, _ := orch.expandLockstepContainers(context.Background(), batch, all, map[string]string{"a": "1.21"})

	assert.Equal(t, []string{"a"}, containerNamesOf(expanded))
}
