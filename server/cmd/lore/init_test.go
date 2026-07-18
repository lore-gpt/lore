package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/lore-gpt/lore/core"
)

type composeFile struct {
	Services map[string]struct {
		Image string `yaml:"image"`
		Build any    `yaml:"build"`
	} `yaml:"services"`
}

func TestRenderComposePinsImageAndIsDeterministic(t *testing.T) {
	const version = "v1.2.3"
	first, err := renderCompose(version)
	if err != nil {
		t.Fatalf("renderCompose: %v", err)
	}
	second, err := renderCompose(version)
	if err != nil {
		t.Fatalf("renderCompose (second): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("renderCompose is not deterministic — two runs of the same version differ")
	}

	var cf composeFile
	if err := yaml.Unmarshal(first, &cf); err != nil {
		t.Fatalf("rendered output is not valid YAML: %v", err)
	}

	want := "ghcr.io/lore-gpt/lore:" + version
	for _, svc := range []string{"lore-server", "lore-worker", "lore-provision"} {
		s, ok := cf.Services[svc]
		if !ok {
			t.Fatalf("service %q missing from generated compose", svc)
		}
		if s.Image != want {
			t.Errorf("service %q image = %q, want %q", svc, s.Image, want)
		}
		if s.Build != nil {
			t.Errorf("service %q should reference the published image, not build from source (got build: %v)", svc, s.Build)
		}
	}
}

// The generated compose pins the dependency images (paradedb/valkey/minio) to the same tags as the
// build-from-source infra/docker-compose.yml. This guards against a Renovate bump touching one file but not
// the other — a version drift would silently give `lore init` users a different stack than contributors.
func TestInitComposeDependencyImagesMatchSource(t *testing.T) {
	rendered, err := renderCompose("v0.0.0")
	if err != nil {
		t.Fatalf("renderCompose: %v", err)
	}
	var initCF composeFile
	if err := yaml.Unmarshal(rendered, &initCF); err != nil {
		t.Fatalf("generated compose is not valid YAML: %v", err)
	}

	srcBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read infra/docker-compose.yml: %v", err)
	}
	var srcCF composeFile
	if err := yaml.Unmarshal(srcBytes, &srcCF); err != nil {
		t.Fatalf("infra/docker-compose.yml is not valid YAML: %v", err)
	}

	for _, svc := range []string{"paradedb", "valkey", "minio"} {
		got, src := initCF.Services[svc].Image, srcCF.Services[svc].Image
		if got == "" || src == "" {
			t.Fatalf("service %q missing an image (init=%q source=%q)", svc, got, src)
		}
		if got != src {
			t.Errorf("dependency %q image drifted — init=%q source=%q (keep them in lockstep)", svc, got, src)
		}
	}
}

// Beyond the dependency-image tags, the lore-* service definitions (command, environment, ports,
// healthcheck, depends_on, restart, user, volumes) must stay identical between the generated compose and the
// build-from-source infra/docker-compose.yml — only the image-vs-build key legitimately differs. Comparing
// everything else catches a contributor updating one compose (e.g. a new env var) but not the other.
func TestInitLoreServiceConfigMatchesSource(t *testing.T) {
	rendered, err := renderCompose("v0.0.0")
	if err != nil {
		t.Fatalf("renderCompose: %v", err)
	}
	srcBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read infra/docker-compose.yml: %v", err)
	}

	initSvc := rawServices(t, rendered)
	srcSvc := rawServices(t, srcBytes)
	for _, svc := range []string{"lore-server", "lore-worker", "lore-provision"} {
		got := withoutImageOrBuild(initSvc[svc])
		want := withoutImageOrBuild(srcSvc[svc])
		if len(got) == 0 || len(want) == 0 {
			t.Fatalf("service %q missing from one of the compose files", svc)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("service %q config drifted (ignoring image/build):\n init=%v\n src =%v\nkeep the two compose files in lockstep", svc, got, want)
		}
	}
}

func rawServices(t *testing.T, data []byte) map[string]map[string]any {
	t.Helper()
	var c struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("unmarshal compose: %v", err)
	}
	return c.Services
}

func withoutImageOrBuild(svc map[string]any) map[string]any {
	out := make(map[string]any, len(svc))
	for k, v := range svc {
		if k == "image" || k == "build" {
			continue
		}
		out[k] = v
	}
	return out
}

func TestInitCmdWritesYAMLToStdoutAndEpilogueToStderr(t *testing.T) {
	cmd := initCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// stdout must be pure YAML — a `> docker-compose.yml` redirect captures exactly this.
	var cf composeFile
	if err := yaml.Unmarshal(out.Bytes(), &cf); err != nil {
		t.Fatalf("stdout is not valid YAML: %v", err)
	}
	if _, ok := cf.Services["lore-server"]; !ok {
		t.Error("stdout compose is missing the lore-server service")
	}
	// The .gitignore warning is epilogue-only (the compose YAML never mentions it), so its absence from
	// stdout proves the epilogue did not leak into the redirect. (The compose's own header comment does
	// legitimately contain "docker compose up", so that phrase can't be used as a leak sentinel.)
	if strings.Contains(out.String(), ".gitignore") {
		t.Error("the human-readable epilogue leaked into stdout — it would corrupt a redirect")
	}

	// The epilogue (next steps + the .gitignore warning) belongs on stderr.
	stderr := errOut.String()
	if !strings.Contains(stderr, "docker compose up") {
		t.Error("stderr epilogue is missing the next-step commands")
	}
	if !strings.Contains(stderr, ".gitignore") {
		t.Error("stderr epilogue should warn about adding .lore/ to .gitignore")
	}
}

func TestRootWiresVersionFlag(t *testing.T) {
	if got := rootCmd().Version; got != core.Version {
		t.Errorf("root command Version = %q, want core.Version %q (enables `lore --version`)", got, core.Version)
	}
}
