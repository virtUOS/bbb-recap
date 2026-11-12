package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/virtUOS/bbb-recap/internal/manifest"
	"github.com/virtUOS/bbb-recap/internal/steps/validate"
)

// pkgVerdict is the output of `recap tool validate-package`
// (contracts/package/README.md "Output").
type pkgVerdict struct {
	Valid     bool            `json:"valid"`
	PackageID *string         `json:"package_id"`
	Files     []manifest.File `json:"files"`
	Gaps      []string        `json:"gaps"`
	Violation *pkgViolation   `json:"violation"`
}

// pkgViolation is an ingest rule ({rule, problem, status}) or a validate
// rule ({rule, job_state, reason}).
type pkgViolation struct {
	Rule     string `json:"rule"`
	Problem  string `json:"problem,omitempty"`
	Status   int    `json:"status,omitempty"`
	JobState string `json:"job_state,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// toolValidatePackage is `recap tool validate-package`: the package rules of
// meeting-package.md §7 on a directory, without configuration or state. It
// reads only the files named on the command line and below DIR.
func toolValidatePackage(args []string, stdout, stderr io.Writer) int {
	const prog = "recap tool validate-package"
	fs := flag.NewFlagSet(prog, flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifestFile := fs.String("manifest", "", "manifest `FILE`; without it DIR is a raw recording directory")
	packageID := fs.String("package-id", "", "package_id from the ingest URL (V-I01)")
	serverID := fs.String("bbb-server-id", "", "server bound to the upload credential (V-I03)")
	jcs := fs.Bool("jcs", false, "write the RFC 8785 canonical form of the JSON on stdin")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *jcs {
		if fs.NArg() > 0 || *manifestFile != "" || *packageID != "" || *serverID != "" {
			fmt.Fprintf(stderr, "%s: --jcs takes no other arguments\n", prog)
			return exitUsage
		}
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(stderr, "%s: read stdin: %v\n", prog, err)
			return exitFailure
		}
		out, err := manifest.Canonicalize(in)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", prog, err)
			return exitFailure
		}
		_, _ = stdout.Write(out)
		return exitOK
	}

	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "Usage: %s [--manifest FILE] [--package-id HEX] [--bbb-server-id ID] DIR\n"+
			"       %s --jcs\n", prog, prog)
		return exitUsage
	}
	dir := fs.Arg(0)
	var v *pkgVerdict
	var err error
	if *manifestFile == "" {
		if *packageID != "" || *serverID != "" {
			fmt.Fprintf(stderr, "%s: --package-id and --bbb-server-id need --manifest\n", prog)
			return exitUsage
		}
		v, err = validateRaw(dir)
	} else {
		v, err = validatePackageForm(dir, *manifestFile, manifest.Options{PackageID: *packageID, BBBServerID: *serverID})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return exitFailure
	}
	v.Valid = v.Violation == nil
	out, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return exitFailure
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", out)
	return exitOK
}

// validateRaw is the raw form: the allow-list selects the files, then
// V-S01 (record_id = directory name), V-S02, V-S03, V-S05.
func validateRaw(dir string) (*pkgVerdict, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	sel, err := manifest.Select(abs)
	if err != nil {
		return nil, fmt.Errorf("select package files: %w", err)
	}
	v := &pkgVerdict{Files: sel.Files, Gaps: []string{}}
	if v.Files == nil {
		v.Files = []manifest.File{}
	}
	err = runRules(v, abs, sel.Files, validate.Package{RecordID: filepath.Base(abs)})
	return v, err
}

// validatePackageForm checks the manifest (V-I01…V-I04, V-I07, V-I08), then
// the files (V-I05), then V-S01…V-S05.
func validatePackageForm(dir, manifestFile string, opts manifest.Options) (*pkgVerdict, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a readable directory", dir)
	}
	data, err := os.ReadFile(manifestFile)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	id := manifest.PackageID(data)
	v := &pkgVerdict{PackageID: &id, Files: []manifest.File{}, Gaps: []string{}}
	m, iv := manifest.Check(data, opts)
	if m != nil && m.Files != nil {
		v.Files = m.Files
	}
	if iv == nil {
		iv, err = manifest.CheckFiles(dir, m.Files)
		if err != nil {
			return nil, fmt.Errorf("check files: %w", err)
		}
	}
	if iv != nil {
		v.Violation = &pkgViolation{Rule: iv.Rule, Problem: iv.Problem, Status: iv.Status}
		return v, nil
	}
	meta := m.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	var mics []string
	for _, f := range m.Files {
		if f.Role == manifest.RoleMicrophone {
			mics = append(mics, f.Path)
		}
	}
	err = runRules(v, dir, m.Files, validate.Package{
		RecordID:    m.RecordID,
		MeetingID:   m.MeetingID,
		Metadata:    meta,
		MetaPrefix:  m.MetaPrefix,
		Microphones: mics,
	})
	return v, err
}

// runRules applies V-S01…V-S05 to the listed files below dir and records
// the result in v. In the raw form p.Microphones is filled from files.
func runRules(v *pkgVerdict, dir string, files []manifest.File, p validate.Package) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if p.Microphones == nil {
		for _, f := range files {
			if f.Role == manifest.RoleMicrophone {
				p.Microphones = append(p.Microphones, f.Path)
			}
		}
	}
	gaps, sv, err := validate.Rules(validate.OnlyFiles(root.FS(), files), p)
	if err != nil {
		return err
	}
	if sv != nil {
		v.Violation = &pkgViolation{Rule: sv.Rule, JobState: sv.JobState(), Reason: sv.Reason}
		return nil
	}
	v.Gaps = gaps
	return nil
}
