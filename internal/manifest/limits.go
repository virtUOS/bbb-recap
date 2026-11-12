package manifest

import "encoding/json"

// Limits of meeting-package.md §8, the same in exporter and service, and the
// manifest body limit of bbb-exporter.md §6.2.
const (
	MaxFileSize     int64 = 2 << 30 // 2 GiB per file
	MaxPackageSize  int64 = 8 << 30 // 8 GiB per package
	MaxFiles              = 10_000  // files per package
	MaxManifestSize       = 1 << 20 // 1 MiB manifest body
)

// CheckLimits checks a file list against the limits of §8 (rule V-E03; at
// ingest 413 package-too-large). The exporter uses it on its selection.
func CheckLimits(files []File) *Violation {
	if len(files) > MaxFiles {
		return tooLarge("%d files, limit %d", len(files), MaxFiles)
	}
	var total int64
	for _, f := range files {
		if f.Size > MaxFileSize {
			return tooLarge("a file has %d bytes, limit %d", f.Size, MaxFileSize)
		}
		total += f.Size
	}
	if total > MaxPackageSize {
		return tooLarge("package has %d bytes, limit %d", total, MaxPackageSize)
	}
	return nil
}

func tooLarge(format string, args ...any) *Violation {
	return violation("V-E03", ProblemPackageTooLarge, 413, format, args...)
}

// checkLimitsLenient applies CheckLimits to manifest bytes that have not
// passed the schema yet, so that a too large package is 413 and not 422. If
// the files cannot be read as a list of integer sizes, it returns nil and
// the schema check reports the problem.
func checkLimitsLenient(data []byte) *Violation {
	var probe struct {
		Files []struct {
			Size json.Number `json:"size"`
		} `json:"files"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return nil
	}
	files := make([]File, 0, len(probe.Files))
	for _, f := range probe.Files {
		n, err := f.Size.Int64()
		if err != nil {
			return nil
		}
		files = append(files, File{Size: n})
	}
	return CheckLimits(files)
}
