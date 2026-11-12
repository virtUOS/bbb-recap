package meeting_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/virtUOS/bbb-recap/"

// pipeline are the packages on the pipeline side of the source boundary
// (ADR-013): they read the normalized meeting only.
var pipeline = []string{
	"internal/meeting",
	"internal/steps/transcribe",
	"internal/steps/assemble",
	"internal/steps/summarize",
	"internal/steps/render",
	"internal/steps/deliver",
	"internal/steps/cleanup",
	"internal/render",
}

// source are the BBB source packages (ADR-013) that no pipeline package may
// depend on, directly or through another package.
var source = []string{
	"internal/events",
	"internal/manifest",
	"internal/ingest",
	"internal/registration",
	"internal/exporter",
	"internal/steps/validate",
	"internal/steps/normalize",
}

// TestSourceBoundary fails when a pipeline package depends on a source
// package, directly or through other packages. It asks the go tool for the
// dependencies (test files excluded) and names each import that crosses
// the boundary.
func TestSourceBoundary(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH: cannot list package dependencies")
	}
	args := []string{"list", "-deps", "-f", `{{.ImportPath}} {{join .Imports " "}}`}
	for _, p := range pipeline {
		args = append(args, module+p)
	}
	cmd := exec.Command(goTool, args...)
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, stderr.String())
	}
	forbidden := map[string]bool{}
	for _, s := range source {
		forbidden[module+s] = true
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg, imports, _ := strings.Cut(line, " ")
		if forbidden[pkg] {
			continue // reported where it is imported from outside the source side
		}
		for _, imp := range strings.Fields(imports) {
			if forbidden[imp] {
				t.Errorf("%s imports the source package %s", pkg, imp)
			}
		}
	}
}
