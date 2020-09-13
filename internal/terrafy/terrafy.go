package terrafy

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"

	"github.com/davecgh/go-spew/spew"
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
	moreDiags = generateProviderRequirements(tmpDir, cfg.ProviderReqs, cfg.SourceFiles)
	diags = append(diags, moreDiags...)
	if moreDiags.HasErrors() {
		// If we couldn't generate the requirements file then the rest of
		// this will not succeed either.
		return cfg.SourceFiles, diags
	}

	err = tf.Init(context.Background())
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to initialize temporary working directory",
			Detail:   fmt.Sprintf("Could not initialize a temporary working directory to handle the import:\n\n%s", err),
		})
		return cfg.SourceFiles, diags
	}

	schemas, err := tf.ProvidersSchema(context.Background())
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to retrieve provider schemas",
			Detail:   fmt.Sprintf("Could not retrieve the schemas for the required providers:\n\n%s", err),
		})
		return cfg.SourceFiles, diags
	}

	spew.Dump(schemas)

	return cfg.SourceFiles, diags
}

func generateProviderRequirements(targetDir string, reqs map[string]hcl.Expression, files map[string]*hcl.File) hcl.Diagnostics {
	// Terraform only installs providers that are actually used by something
	// in the configuration, so we'll also generate a temporary file
	// with some placeholder "provider" blocks to force the installation
	// of each provider. We'll overwrite providers.tf with a real set of
	// provider configurations in a later step, once we've got all the
	// providers installed so we can see their schemas.
	versionsFilename := filepath.Join(targetDir, "versions.tf")
	providersFilename := filepath.Join(targetDir, "providers.tf")
	var diags hcl.Diagnostics
	versionsFile := hclwrite.NewEmptyFile()
	providersFile := hclwrite.NewEmptyFile()

	tfBlock := versionsFile.Body().AppendNewBlock("terraform", nil)
	reqsBlock := tfBlock.Body().AppendNewBlock("required_providers", nil)
	names := make([]string, 0, len(reqs))

	for name := range reqs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		expr := reqs[name]
		val, moreDiags := expr.Value(nil)
		diags = append(diags, moreDiags...)
		if moreDiags.HasErrors() {
			continue
		}
		reqsBlock.Body().SetAttributeValue(name, val)

		providersFile.Body().AppendNewBlock("provider", []string{name})
	}

	err := ioutil.WriteFile(versionsFilename, versionsFile.Bytes(), 0700)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to create provider requirements file",
			Detail:   fmt.Sprintf("Could not create a temporary provider requirements file: %s.", err),
		})
		return diags
	}

	err = ioutil.WriteFile(providersFilename, providersFile.Bytes(), 0700)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to create provider configurations file",
			Detail:   fmt.Sprintf("Could not create a temporary provider configurations file: %s.", err),
		})
		return diags
	}

	return diags
}
