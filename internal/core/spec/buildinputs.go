package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// BuildInputs is a digest of everything in a spec that decides what a build
// produces, and nothing else: where the source is read from, how it is built,
// with which arguments and under which constraints, by which builder, for
// which runtime, and each workload's own build. A spec whose BuildInputs are
// unchanged builds the same thing from the same commit.
//
// It is how a deploy knows the image detection built is the image it would
// build (R-311, design 09 §4.1): same source, same inputs, so the image and its
// scan are reused rather than built a second time. Anything else in the spec —
// ports, environment, routing, limits — changes how the image runs, not what
// it is, and is left out so that answering a port does not cost a rebuild.
//
// The commit is not part of it: the caller compares the checkout it builds
// from, which names an upload by its digest as well as a repository by its
// commit.
func BuildInputs(s *AppSpec) string {
	if s == nil {
		return ""
	}
	type workloadBuild struct {
		Name  string         `json:"name"`
		Build *WorkloadBuild `json:"build,omitempty"`
	}
	in := struct {
		SourceType SourceType      `json:"source_type"`
		URL        string          `json:"url,omitempty"`
		Subdir     string          `json:"subdir,omitempty"`
		UploadID   string          `json:"upload_id,omitempty"`
		Build      Build           `json:"build"`
		Runtime    string          `json:"runtime"`
		Start      []string        `json:"start,omitempty"`
		Workloads  []workloadBuild `json:"workloads,omitempty"`
	}{
		SourceType: s.Source.Type,
		URL:        s.Source.URL,
		Subdir:     s.Source.Subdir,
		UploadID:   s.Source.UploadID,
		Build:      s.Build,
		Runtime:    s.Runtime.AdapterRef,
	}
	// How long a build may take does not change what it produces.
	in.Build.TimeoutSeconds = 0
	// The arguments by name, so the same arguments in another order are the
	// same build.
	in.Build.Args = sortedArgs(s.Build.Args)

	// A buildpack build is planned around the start command a person gave;
	// every other strategy takes it from the image.
	if s.Build.Strategy == BuildBuildpack {
		if primary, ok := s.PrimaryWorkload(); ok {
			in.Start = primary.Command
		}
	}
	for _, w := range s.Workloads {
		if w.Build == nil {
			continue
		}
		b := *w.Build
		b.Args = sortedArgs(b.Args)
		in.Workloads = append(in.Workloads, workloadBuild{Name: w.Name, Build: &b})
	}
	sort.Slice(in.Workloads, func(i, j int) bool { return in.Workloads[i].Name < in.Workloads[j].Name })

	raw, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func sortedArgs(args []KV) []KV {
	if len(args) == 0 {
		return nil
	}
	out := append([]KV(nil), args...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
