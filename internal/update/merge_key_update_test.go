package update

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chis/docksmith/internal/docker"
	"github.com/chis/docksmith/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A service whose image is inherited through a YAML merge key (`<<: *anchor`)
// is updated by rewriting the anchor's image node, so the stack keeps a single
// source of truth for a lockstep image.
func TestUpdateBatchComposeFiles_FollowsMergeKeyToAnchor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	composeFile := filepath.Join(dir, "gramps.yaml")
	require.NoError(t, os.WriteFile(composeFile, []byte(`services:
  grampsweb: &grampsweb
    image: ghcr.io/gramps-project/grampsweb:2.3.0
    container_name: grampsweb
  grampsweb_celery:
    <<: *grampsweb
    container_name: grampsweb_celery
`), 0644))

	containers := []*docker.Container{
		{
			Name:  "grampsweb_celery",
			Image: "ghcr.io/gramps-project/grampsweb:2.3.0",
			Labels: map[string]string{
				"com.docker.compose.project.config_files": composeFile,
				"com.docker.compose.service":              "grampsweb_celery",
			},
		},
	}
	targetVersions := map[string]string{"grampsweb_celery": "2.4.0"}

	store := NewTestMockStorage()
	store.operations["op-1"] = storage.UpdateOperation{
		OperationID:   "op-1",
		OperationType: "batch",
		StackName:     "intimidator",
		BatchDetails: []storage.BatchContainerDetail{
			{ContainerName: "grampsweb_celery", Status: "pending"},
		},
	}
	orch := &UpdateOrchestrator{storage: store}

	failed := orch.updateBatchComposeFiles(ctx, "op-1", "intimidator", containers, targetVersions)
	assert.Empty(t, failed, "an inherited image should be updatable through its anchor")

	content, err := os.ReadFile(composeFile)
	require.NoError(t, err)
	assert.Contains(t, string(content), "image: ghcr.io/gramps-project/grampsweb:2.4.0",
		"the anchor's image should carry the new tag")
	assert.Contains(t, string(content), "<<: *grampsweb",
		"the merge key should be preserved")

	op, found, err := store.GetUpdateOperation(ctx, "op-1")
	require.NoError(t, err)
	require.True(t, found)
	assert.NotEqual(t, "failed", op.BatchDetails[0].Status)
}
