// Package chartrender renders the repository's Helm chart in process.
//
// It exists so that the guardrail tests and the e2e suite install the chart
// itself rather than a copy of its output: `deploy/*.yaml` were the operator's
// reference and the e2e's hand-parsed input at once, with nothing checking they
// agreed (ADR 0036). Values pass through the chart's JSON schema, so invalid
// values fail the way an operator's `helm install` would.
package chartrender

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"helm.sh/helm/v4/pkg/chart/common"
	commonutil "helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/engine"
)

// Dir is the chart's location relative to the repository root.
const Dir = "charts/runtime-agent"

// Options are the parts of an install that change what renders.
type Options struct {
	// ReleaseName defaults to "runtime-agent".
	ReleaseName string
	// Namespace is the release namespace, which the chart writes into the
	// node's expected subject and the controller's in-cluster endpoint.
	Namespace string
	// Values overlay the chart's defaults, in the shape of values.yaml.
	Values map[string]any
}

// Render returns the chart's manifests keyed by template path. Templates that
// render to nothing under the given values are dropped, so the result names
// exactly what an install would create.
func Render(chartDir string, opts Options) (map[string]string, error) {
	chrt, err := loader.LoadDir(chartDir)
	if err != nil {
		return nil, fmt.Errorf("loading chart %s: %w", chartDir, err)
	}
	if opts.ReleaseName == "" {
		opts.ReleaseName = "runtime-agent"
	}
	if opts.Namespace == "" {
		opts.Namespace = "runtime-agent"
	}
	values := opts.Values
	if values == nil {
		values = map[string]any{}
	}
	rendered, err := commonutil.ToRenderValuesWithSchemaValidation(chrt, values,
		common.ReleaseOptions{
			Name:      opts.ReleaseName,
			Namespace: opts.Namespace,
			Revision:  1,
			IsInstall: true,
		},
		common.DefaultCapabilities, false)
	if err != nil {
		return nil, fmt.Errorf("resolving values: %w", err)
	}
	out, err := engine.Render(chrt, rendered)
	if err != nil {
		return nil, fmt.Errorf("rendering chart: %w", err)
	}
	// Partials and templates whose whole body is disabled render to whitespace.
	// An operator never sees those, so neither should a caller.
	//
	// NOTES.txt is dropped for a different reason: Helm renders it like any
	// other template but prints it to the installer instead of applying it, so
	// it is not a manifest and decoding it as YAML fails in whatever reads this.
	for path, body := range out {
		base := filepath.Base(path)
		if strings.TrimSpace(body) == "" || strings.HasPrefix(base, "_") || base == "NOTES.txt" {
			delete(out, path)
		}
	}
	return out, nil
}

// Manifests returns every rendered YAML document, in a deterministic order.
func Manifests(chartDir string, opts Options) ([]string, error) {
	rendered, err := Render(chartDir, opts)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(rendered))
	for path := range rendered {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var docs []string
	for _, path := range paths {
		for _, doc := range strings.Split(rendered[path], "\n---") {
			if strings.TrimSpace(stripComments(doc)) == "" {
				continue
			}
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

// stripComments removes whole-line comments, so a document that is nothing but
// the chart's explanatory header is recognized as empty.
func stripComments(doc string) string {
	var kept []string
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
