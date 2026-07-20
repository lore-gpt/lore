package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/lore-gpt/lore/core"
)

// composeTemplate is the image-based local topology emitted by `lore init`. It mirrors
// infra/docker-compose.yml (the build-from-source variant used by contributors) but pins the lore services to
// a published image instead of building. Keep the dependency image tags (paradedb/valkey/minio) in lockstep
// with infra/docker-compose.yml — a test asserts they match so a Renovate bump can't drift them apart.
//
//go:embed compose.tmpl.yaml
var composeTemplate string

// imageRepo is the published image repository. `lore init` pins the generated compose to THIS binary's
// version (core.Version), so the image that runs `init` and the image the generated compose references can
// never drift apart.
const imageRepo = "ghcr.io/lore-gpt/lore"

// inspectorImageRepo is the published diagnostics-UI image. It releases in lockstep with imageRepo on the same
// version tag, so `lore init` pins both to this binary's version and the two can never drift apart.
const inspectorImageRepo = "ghcr.io/lore-gpt/lore-inspector"

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Print a docker-compose file for a local Lore stack to stdout",
		Long: "Print a docker-compose file for a local, self-hosted Lore stack to stdout. Redirect it to a " +
			"file and bring the stack up:\n\n" +
			"  docker run --rm ghcr.io/lore-gpt/lore:<version> init > docker-compose.yml\n" +
			"  docker compose up --wait\n" +
			"  cat ./.lore/credentials\n\n" +
			"The generated compose is pinned to this image's version. stdout is pure YAML; all human-readable " +
			"output (next steps, warnings) goes to stderr, so the redirect stays clean.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rendered, err := renderCompose(core.Version)
			if err != nil {
				return err
			}
			if _, err := cmd.OutOrStdout().Write(rendered); err != nil {
				return err
			}
			// Human-readable epilogue goes to stderr so a `> docker-compose.yml` redirect captures only YAML.
			_, err = fmt.Fprint(cmd.ErrOrStderr(), initEpilogue)
			return err
		},
	}
}

// renderCompose renders the compose template pinned to the published image at the given version. The output is
// deterministic — it depends only on `version`, with no timestamp or randomness — so two runs of the same
// version produce byte-identical files, and it contains ONLY YAML, safe to redirect to a file.
func renderCompose(version string) ([]byte, error) {
	tmpl, err := template.New("compose").Parse(composeTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse compose template: %w", err)
	}
	var buf bytes.Buffer
	data := struct{ Image, InspectorImage string }{
		Image:          imageRepo + ":" + version,
		InspectorImage: inspectorImageRepo + ":" + version,
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render compose template: %w", err)
	}
	return buf.Bytes(), nil
}

const initEpilogue = `
Wrote a docker-compose file pinned to this image version. Next:

  docker compose up --wait     # start the stack and provision a first project
  cat ./.lore/credentials      # your project id and API key

The stack writes credentials to ./.lore/credentials. If this directory is inside a
git repository, add ".lore/" to your .gitignore so the key is never committed.
`
