package security

import "github.com/trypando/pando/internal/adapter/api"

// Union is one scan's result from scans of its parts: the image's findings
// and the source's, in that order, with a finding both report kept once.
//
// What a scanner given an image and a source together reports (design 09 §3):
// a dependency found in both costs what it costs once. Two findings are the
// same finding when they have the same ID at the same target — the fields
// every scanner fills, so nothing here reads one scanner's vocabulary (R-251).
// The scanner named is the parts' own, and the time is when the last part
// finished.
func Union(parts ...api.ScanResult) api.ScanResult {
	var out api.ScanResult
	seen := map[string]bool{}
	for _, part := range parts {
		if out.Scanner == "" {
			out.Scanner = part.Scanner
		}
		if part.Ran.After(out.Ran) {
			out.Ran = part.Ran
		}
		for _, f := range part.Findings {
			key := f.ID + "\x00" + f.Target
			if seen[key] {
				continue
			}
			seen[key] = true
			out.Findings = append(out.Findings, f)
		}
	}
	return out
}
