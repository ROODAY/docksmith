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

// A single container whose compose file cannot be edited (e.g. its image comes
// from a YAML merge key, so the service has no literal image field) must not
// abort the compose-update phase for the rest of the batch.
func TestUpdateBatchComposeFiles_OneBadServiceDoesNotAbortBatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// stack-a: web declares its image literally; celery is build-only, so it has
	// no image field anywhere and cannot be rewritten.
	fileA := filepath.Join(dir, "stack-a.yaml")
	require.NoError(t, os.WriteFile(fileA, []byte(`services:
  web:
    image: nginx:1.20
    container_name: web
  celery:
    build: .
    container_name: celery
`), 0644))

	// stack-b: an unrelated, perfectly fine service in a separate file.
	fileB := filepath.Join(dir, "stack-b.yaml")
	require.NoError(t, os.WriteFile(fileB, []byte(`services:
  api:
    image: api:1.0
    container_name: api
`), 0644))

	labels := func(file, service string) map[string]string {
		return map[string]string{
			"com.docker.compose.project.config_files": file,
			"com.docker.compose.service":              service,
		}
	}
	containers := []*docker.Container{
		{Name: "web", Image: "nginx:1.20", Labels: labels(fileA, "web")},
		{Name: "celery", Image: "nginx:1.20", Labels: labels(fileA, "celery")},
		{Name: "api", Image: "api:1.0", Labels: labels(fileB, "api")},
	}
	targetVersions := map[string]string{"web": "1.21", "celery": "1.21", "api": "1.1"}

	store := NewTestMockStorage()
	store.operations["op-1"] = storage.UpdateOperation{
		OperationID:   "op-1",
		OperationType: "batch",
		StackName:     "intimidator",
		BatchDetails: []storage.BatchContainerDetail{
			{ContainerName: "web", Status: "pending"},
			{ContainerName: "celery", Status: "pending"},
			{ContainerName: "api", Status: "pending"},
		},
	}
	orch := &UpdateOrchestrator{storage: store}

	failed := orch.updateBatchComposeFiles(ctx, "op-1", "intimidator", containers, targetVersions)

	assert.Equal(t, map[string]bool{"celery": true}, failed,
		"only the container whose compose edit failed should be reported")

	contentA, err := os.ReadFile(fileA)
	require.NoError(t, err)
	assert.Contains(t, string(contentA), "image: nginx:1.21",
		"web's compose entry should still be updated despite celery failing")

	contentB, err := os.ReadFile(fileB)
	require.NoError(t, err)
	assert.Contains(t, string(contentB), "image: api:1.1",
		"an unrelated stack in the same batch should still be updated")

	op, found, err := store.GetUpdateOperation(ctx, "op-1")
	require.NoError(t, err)
	require.True(t, found)

	statuses := make(map[string]string, len(op.BatchDetails))
	messages := make(map[string]string, len(op.BatchDetails))
	for _, d := range op.BatchDetails {
		statuses[d.ContainerName] = d.Status
		messages[d.ContainerName] = d.Message
	}
	assert.Equal(t, "failed", statuses["celery"])
	assert.Contains(t, messages["celery"], "has no image field")
	assert.NotEqual(t, "failed", statuses["web"], "web should not be marked failed")
	assert.NotEqual(t, "failed", statuses["api"], "api should not be marked failed")
}
