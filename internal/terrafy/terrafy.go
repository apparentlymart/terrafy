package terrafy

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
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

	_, err = tf.ProvidersSchema(context.Background())
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to retrieve provider schemas",
			Detail:   fmt.Sprintf("Could not retrieve the schemas for the required providers:\n\n%s", err),
		})
		return cfg.SourceFiles, diags
	}

	// Now we'll generate the rest of our temporary Terraform configuration
	// to prepare the data (from the data resources) we need to complete the
	// import.
	// Note that this now overwrites the stub provider configurations we
	// generated above just to prompt Terraform to produce the schemas,
	// now to include the actual configuration provided by the user just
	// in case the data resources need them.
	moreDiags = generatePrepConfig(tmpDir, cfg)
	diags = append(diags, moreDiags...)
	if moreDiags.HasErrors() {
		return cfg.SourceFiles, diags
	}

	err = tf.Apply(context.Background())
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to read data resources",
			Detail:   fmt.Sprintf("Could not read the defined data resources to prepare for import:\n\n%s", err),
		})
		return cfg.SourceFiles, diags
	}

	state, err := tf.Show(context.Background())
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to read data results",
			Detail:   fmt.Sprintf("Could not read the data resource results:\n\n%s", err),
		})
		return cfg.SourceFiles, diags
	}
	idsRaw := state.Values.Outputs["ids"].Value
	spew.Dump(idsRaw)

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

func generatePrepConfig(targetDir string, cfg *Config) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// We're going to copy raw chunks of configuration byte-for-byte
	// from the input into the temporary config, but that means we
	// can't support JSON because we can't just paste that verbatim
	// into a .tf file. (Maybe later we'll generate a sidecar .tf.json
	// file, but the HCL JSON syntax makes it harder to reliably
	// extract a suitable raw chunk of configuration due to the
	// different variants it supports for nested block representation.)
	providersNativeFilename := filepath.Join(targetDir, "providers.tf")
	providersNativeFile := hclwrite.NewEmptyFile()
	dataNativeFilename := filepath.Join(targetDir, "data.tf")
	dataNativeFile := hclwrite.NewEmptyFile()

	for key, block := range cfg.ProviderConfigs {
		name := key
		if dot := strings.Index(key, "."); dot >= 0 {
			name = key[:dot]
		}
		body, ok := block.Body.(*hclsyntax.Body)
		if !ok {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  "Skipping provider configuration declared in JSON",
				Detail:   fmt.Sprintf("Terrafy cannot currently incorporate provider configurations from .tf.json files.\n\nIf your Terrafy configuration depends on provider %s then the import may fail. Reproduce the provider configuration in one of your .tfy files in native syntax, if so.", key),
				Subject:  block.DefRange.Ptr(),
			})
			continue
		}
		sourceFileBytes := cfg.SourceFiles[body.SrcRange.Filename].Bytes
		sourceBytes := body.SrcRange.SliceBytes(sourceFileBytes)
		sourceBytes = sourceBytes[1 : len(sourceBytes)-1] // strip the braces
		tmpFile, moreDiags := hclwrite.ParseConfig(sourceBytes, "", hcl.InitialPos)
		diags = append(diags, moreDiags...)
		if moreDiags.HasErrors() {
			// It'd be weird to get here after having previously parsed the
			// whole file successfully, but we'll be robust anyway.
			continue
		}
		newBlock := providersNativeFile.Body().AppendNewBlock("provider", []string{name})
		newBlock.Body().AppendUnstructuredTokens(tmpFile.Body().BuildTokens(nil))
	}

	err := ioutil.WriteFile(providersNativeFilename, providersNativeFile.Bytes(), 0700)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to create provider configurations file",
			Detail:   fmt.Sprintf("Could not create a temporary provider configurations file: %s.", err),
		})
		return diags
	}

	for addr, block := range cfg.DataResources {
		body, ok := block.Body.(*hclsyntax.Body)
		if !ok {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  "Skipping data resource configuration not in native syntax",
				Detail:   "Terrafy can currently only incorporate data resource configurations from Terraform files.",
				Subject:  block.DefRange.Ptr(),
			})
			continue
		}
		sourceFileBytes := cfg.SourceFiles[body.SrcRange.Filename].Bytes
		sourceBytes := body.SrcRange.SliceBytes(sourceFileBytes)
		sourceBytes = sourceBytes[1 : len(sourceBytes)-1] // strip the braces
		tmpFile, moreDiags := hclwrite.ParseConfig(sourceBytes, "", hcl.InitialPos)
		diags = append(diags, moreDiags...)
		if moreDiags.HasErrors() {
			// It'd be weird to get here after having previously parsed the
			// whole file successfully, but we'll be robust anyway.
			continue
		}
		newBlock := dataNativeFile.Body().AppendNewBlock("data", []string{addr.Type, addr.Name})
		newBlock.Body().AppendUnstructuredTokens(tmpFile.Body().BuildTokens(nil))
	}

	var hackyOutputTokens hclwrite.Tokens
	hackyOutputTokens = append(hackyOutputTokens, &hclwrite.Token{
		Type:  hclsyntax.TokenOBrace,
		Bytes: []byte{'{'},
	})
	hackyOutputTokens = append(hackyOutputTokens, &hclwrite.Token{
		Type:  hclsyntax.TokenNewline,
		Bytes: []byte{'\n'},
	})
	for addr, imp := range cfg.ImportConfigs {
		hackyOutputTokens = append(hackyOutputTokens, &hclwrite.Token{
			Type:  hclsyntax.TokenOQuote,
			Bytes: []byte{'"'},
		})
		hackyOutputTokens = append(hackyOutputTokens, &hclwrite.Token{
			Type:  hclsyntax.TokenQuotedLit,
			Bytes: []byte(addr.String()),
		})
		hackyOutputTokens = append(hackyOutputTokens, &hclwrite.Token{
			Type:  hclsyntax.TokenCQuote,
			Bytes: []byte{'"'},
		})
		hackyOutputTokens = append(hackyOutputTokens, &hclwrite.Token{
			Type:  hclsyntax.TokenEqual,
			Bytes: []byte{'='},
		})
		sourceRange := imp.ID.Range()
		sourceFileBytes := cfg.SourceFiles[sourceRange.Filename].Bytes
		sourceBytes := sourceRange.SliceBytes(sourceFileBytes)
		// EVIL: here we're exploiting the fact that we don't actually
		// intend to re-access the tokens here, and this input is not
		// intended for human consumption anyway so we don't need to worry
		// too much about canonical formatting, and so we can lie to hclwrite
		// and pretend these whole buffers are single tokens... they'll just
		// get concatenated as-is into the output anyway. Perhaps one day
		// hclwrite will have a function for parsing an isolated expression,
		// but it doesn't today.
		hackyOutputTokens = append(hackyOutputTokens, &hclwrite.Token{
			Type:  hclsyntax.TokenInvalid,
			Bytes: sourceBytes,
		})
		hackyOutputTokens = append(hackyOutputTokens, &hclwrite.Token{
			Type:  hclsyntax.TokenNewline,
			Bytes: []byte{'\n'},
		})
	}
	hackyOutputTokens = append(hackyOutputTokens, &hclwrite.Token{
		Type:  hclsyntax.TokenCBrace,
		Bytes: []byte{'}'},
	})
	newBlock := dataNativeFile.Body().AppendNewBlock("output", []string{"ids"})
	newBlock.Body().SetAttributeRaw("value", hackyOutputTokens)

	err = ioutil.WriteFile(dataNativeFilename, dataNativeFile.Bytes(), 0700)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to create data configuration file",
			Detail:   fmt.Sprintf("Could not create a temporary data configuration file: %s.", err),
		})
		return diags
	}

	return diags
}
