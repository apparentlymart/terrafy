package terrafy

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/terraform-exec/tfexec"
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

	tmpDir, err := ioutil.TempDir("", "terrafy-")
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to create temporary directory",
			Detail:   fmt.Sprintf("Could not create a temporary working directory: %s.", err),
		})
		return cfg.SourceFiles, diags
	}
	defer os.RemoveAll(tmpDir)

	tf, err := tfexec.NewTerraform(tmpDir, opts.TerraformExec)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to initialize Terraform CLI",
			Detail:   fmt.Sprintf("Terraform executable at %s is malfunctioning or not available: %s.", opts.TerraformExec, err),
		})
		return cfg.SourceFiles, diags
	}

	// First we need to get all of the required providers installed, so we can
	// read their schemas in preparation for our later work.
	err = generateProviderRequirements(filepath.Join(tmpDir, "versions.tf"), cfg.ProviderReqs, cfg.SourceFiles)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to create provider requirements file",
			Detail:   fmt.Sprintf("Could not create a temporary provider requirements file: %s.", err),
		})
		return cfg.SourceFiles, diags
	}

	err = tf.Init(context.Background())
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to initialize temporary working directory",
			Detail:   fmt.Sprintf("Could not initialize a temporary working directory to handle the import: %s.", err),
		})
		return cfg.SourceFiles, diags
	}

	return cfg.SourceFiles, diags
}

func generateProviderRequirements(targetFile string, reqs map[string]hcl.Expression, files map[string]*hcl.File) error {
	f := hclwrite.NewEmptyFile()

	return ioutil.WriteFile(targetFile, f.Bytes(), 0700)
}
