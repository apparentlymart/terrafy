package terrafy

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/hashicorp/hcl/v2"
)

// Options represents execution options that are customizable from the
// command line.
type Options struct {
	TerraformExec string
}

// Run is the main entrypoint.
//
// It returns a map of the source code of any files it used as part of its
// work, along with any diagnostics.
func Run(opts *Options) (map[string]*hcl.File, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	cfg, moreDiags := LoadConfig(".")
	diags = append(diags, moreDiags...)
	if moreDiags.HasErrors() {
		return cfg.SourceFiles, diags
	}

	spew.Dump(cfg)

	return nil, diags
}
