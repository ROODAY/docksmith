package compose

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServicesSharingImage(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		service     string
		wantSharing []string
	}{
		{
			name: "services inheriting the same anchor share their image",
			yaml: `services:
  grampsweb: &grampsweb
    image: ghcr.io/gramps-project/grampsweb:latest
  grampsweb_celery:
    <<: *grampsweb
  other:
    image: redis:7
`,
			service:     "grampsweb_celery",
			wantSharing: []string{"grampsweb", "grampsweb_celery"},
		},
		{
			name: "identical literal images are not a shared anchor",
			yaml: `services:
  a:
    image: nginx:1.20
  b:
    image: nginx:1.20
`,
			service:     "a",
			wantSharing: []string{"a"},
		},
		{
			name: "explicit image overrides the anchor and is not shared",
			yaml: `services:
  base: &base
    image: base:1.0
  overridden:
    <<: *base
    image: overridden:2.0
`,
			service:     "overridden",
			wantSharing: []string{"overridden"},
		},
		{
			name: "service without an image shares nothing",
			yaml: `services:
  builder:
    build: .
  other:
    build: .
`,
			service:     "builder",
			wantSharing: []string{"builder"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile := createTempComposeFile(t, tt.yaml)
			cf, err := LoadComposeFile(tmpFile)
			require.NoError(t, err)

			svc, err := cf.FindServiceByContainerName(tt.service)
			require.NoError(t, err)

			assert.ElementsMatch(t, tt.wantSharing, cf.ServicesSharingImage(svc))
		})
	}
}

func TestGetServiceImage_FollowsMergeKeys(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		service  string
		expected string
	}{
		{
			name: "literal image",
			yaml: `services:
  web:
    image: nginx:1.20
`,
			service:  "web",
			expected: "nginx:1.20",
		},
		{
			name: "image inherited from a merge key",
			yaml: `services:
  web: &web
    image: ghcr.io/gramps-project/grampsweb:latest
  celery:
    <<: *web
`,
			service:  "celery",
			expected: "ghcr.io/gramps-project/grampsweb:latest",
		},
		{
			name: "explicit image overrides the merge key",
			yaml: `services:
  web: &web
    image: base:1.0
  celery:
    <<: *web
    image: celery:2.0
`,
			service:  "celery",
			expected: "celery:2.0",
		},
		{
			name: "merge chain",
			yaml: `services:
  a: &a
    image: chained:3.0
  b: &b
    <<: *a
  c:
    <<: *b
`,
			service:  "c",
			expected: "chained:3.0",
		},
		{
			name: "sequence of merges, first with an image wins",
			yaml: `services:
  a: &a
    restart: always
  b: &b
    image: second:2.0
  c: &c
    image: third:3.0
  target:
    <<: [*a, *b, *c]
`,
			service:  "target",
			expected: "second:2.0",
		},
		{
			name: "no image anywhere",
			yaml: `services:
  builder:
    build: .
`,
			service:  "builder",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile := createTempComposeFile(t, tt.yaml)
			cf, err := LoadComposeFile(tmpFile)
			require.NoError(t, err)

			svc, err := cf.FindServiceByContainerName(tt.service)
			require.NoError(t, err)

			assert.Equal(t, tt.expected, GetServiceImage(svc))
		})
	}
}
