package main

import (
	"bytes"
	"fmt"
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

	// The inspector is a SEPARATE published image (ghcr.io/lore-gpt/lore-inspector), released in lockstep on
	// the same tag, so `lore init` pins both to one version.
	insp, ok := cf.Services["lore-inspector"]
	if !ok {
		t.Fatal("service \"lore-inspector\" missing from generated compose")
	}
	if wantInsp := "ghcr.io/lore-gpt/lore-inspector:" + version; insp.Image != wantInsp {
		t.Errorf("lore-inspector image = %q, want %q", insp.Image, wantInsp)
	}
	if insp.Build != nil {
		t.Errorf("lore-inspector should reference the published image, not build from source (got build: %v)", insp.Build)
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
	for _, svc := range []string{"lore-server", "lore-worker", "lore-provision", "lore-inspector"} {
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

// TestComposeWorkerCarriesItsOwnConfigBridges pins the environment the worker actually reads.
//
// The lockstep test above only proves the two compose files AGREE; it stays green if both drop a
// variable at once. That is exactly what happened: extraction and the working-memory stripe are
// configured in `lore worker` (see workerCmd — extraction.Build and workmem.Open both read the
// worker's env), but neither compose passed them, so `LORE_EXTRACTION_PROVIDER=anthropic` before
// `up` changed nothing and the worker ran with the cache disabled while /healthz — which reports
// the SERVER's view — still read "ok". Both failures were silent.
//
// So assert presence, not just agreement. Embedding is included because the worker embeds stored
// memories: if it and serve ever build different embedders, the query and the vectors land in
// different spaces.
func TestComposeWorkerCarriesItsOwnConfigBridges(t *testing.T) {
	rendered, err := renderCompose("v0.0.0")
	if err != nil {
		t.Fatalf("renderCompose: %v", err)
	}
	srcBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read infra/docker-compose.yml: %v", err)
	}

	// Every variable workerCmd reads, and how the compose must supply it: a literal value for the
	// in-network service address, a host-env passthrough for anything an operator opts into.
	want := map[string]string{
		"LORE_VALKEY_URL":                "redis://valkey:6379",
		"LORE_EXTRACTION_PROVIDER":       "${LORE_EXTRACTION_PROVIDER:-}",
		"LORE_EXTRACTION_MODEL":          "${LORE_EXTRACTION_MODEL:-}",
		"ANTHROPIC_API_KEY":              "${ANTHROPIC_API_KEY:-}",
		"LORE_EMBEDDING_PROVIDER":        "${LORE_EMBEDDING_PROVIDER:-}",
		"LORE_EMBEDDING_MODEL":           "${LORE_EMBEDDING_MODEL:-}",
		"LORE_EMBEDDING_DIM":             "${LORE_EMBEDDING_DIM:-}",
		"LORE_EMBEDDING_BASE_URL":        "${LORE_EMBEDDING_BASE_URL:-}",
		"LORE_EMBEDDING_SEND_DIMENSIONS": "${LORE_EMBEDDING_SEND_DIMENSIONS:-}",
		"LORE_EMBEDDING_API_KEY":         "${LORE_EMBEDDING_API_KEY:-}",
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"generated by lore init", rendered},
		{"infra/docker-compose.yml", srcBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := serviceEnv(t, tc.data, "lore-worker")
			for key, val := range want {
				got, ok := env[key]
				if !ok {
					t.Errorf("lore-worker is missing %s — the worker reads it, so an operator setting it sees no effect", key)
					continue
				}
				if got != val {
					t.Errorf("lore-worker %s = %q, want %q", key, got, val)
				}
			}
		})
	}

	// The stripe address must be the SAME one serve uses, or the two halves write to different caches.
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"generated by lore init", rendered},
		{"infra/docker-compose.yml", srcBytes},
	} {
		t.Run(tc.name+" (worker and server share one stripe)", func(t *testing.T) {
			w := serviceEnv(t, tc.data, "lore-worker")["LORE_VALKEY_URL"]
			s := serviceEnv(t, tc.data, "lore-server")["LORE_VALKEY_URL"]
			if w != s {
				t.Errorf("LORE_VALKEY_URL differs — worker=%q server=%q; they must address one stripe", w, s)
			}
		})
	}
}

// serviceEnv reads a service's environment mapping as plain strings.
func serviceEnv(t *testing.T, data []byte, service string) map[string]string {
	t.Helper()
	svc, ok := rawServices(t, data)[service]
	if !ok {
		t.Fatalf("service %q missing from the compose file", service)
	}
	raw, ok := svc["environment"].(map[string]any)
	if !ok {
		t.Fatalf("service %q has no mapping-style environment block", service)
	}
	env := make(map[string]string, len(raw))
	for k, v := range raw {
		env[k] = fmt.Sprint(v)
	}
	return env
}

// composePorts parses just the ports of each service, for assertions about what a generated compose
// publishes to the host.
type composePorts struct {
	Services map[string]struct {
		Ports []string `yaml:"ports"`
	} `yaml:"services"`
}

// TestComposeDoesNotPublishTheMetricsPort locks a promise the documentation makes.
//
// /metrics is unauthenticated and now listens on its own port. That is only an improvement if the shipped
// compose leaves that port inside the network — publishing it would put the endpoint exactly where the API
// is, which is the situation the separate listener exists to avoid. Nothing else would catch a stray
// mapping: the drift test only compares the two compose files to each other, so adding the same wrong line
// to both would keep it quiet.
func TestComposeDoesNotPublishTheMetricsPort(t *testing.T) {
	rendered, err := renderCompose(core.Version)
	if err != nil {
		t.Fatalf("renderCompose: %v", err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read infra compose: %v", err)
	}

	for name, raw := range map[string][]byte{"generated": rendered, "infra": source} {
		var cf composePorts
		if err := yaml.Unmarshal(raw, &cf); err != nil {
			t.Fatalf("%s compose is not valid YAML: %v", name, err)
		}
		// The API port IS published, so a compose that published nothing at all would pass this test
		// vacuously. Assert it is there before asserting what is not.
		srv, ok := cf.Services["lore-server"]
		if !ok {
			t.Fatalf("%s: lore-server missing", name)
		}
		publishesAPI := false
		for _, p := range srv.Ports {
			if strings.HasSuffix(p, ":8080") {
				publishesAPI = true
			}
		}
		if !publishesAPI {
			t.Fatalf("%s: lore-server publishes no API port; the check below would prove nothing", name)
		}

		for _, svc := range []string{"lore-server", "lore-worker"} {
			s := cf.Services[svc]
			for _, p := range s.Ports {
				if strings.Contains(p, "9090") {
					t.Errorf("%s: %s publishes %q — the metrics port must stay inside the network, since the endpoint is unauthenticated",
						name, svc, p)
				}
			}
		}
	}
}
