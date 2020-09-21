package terrafy

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	hcl "github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/zclconf/go-cty/cty"
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
	idsRaw := state.Values.Outputs["ids"].Value.(map[string]interface{})

	// We've now completed our work with the temporary directory: we've read all
	// of the data resources and evaluated all of the "id" arguments in the
	// import blocks. The rest of our work will be with the main configuration
	// in the directory where we were run.
	tf, err = tfexec.NewTerraform(".", opts.TerraformExec)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to initialize Terraform CLI",
			Detail:   fmt.Sprintf("Terraform executable at %s is malfunctioning or not available: %s.", opts.TerraformExec, err),
		})
		return cfg.SourceFiles, diags
	}

	// Now our task is to visit each of the resource instances the user
	// declared to import and see whether each one is already accounted for
	// in the state (if not, we'll import it) and in the configuration
	// (if not, we'll generate it from what's in the state).
	plan, moreDiags := planImporting(cfg, idsRaw, tf)
	diags = append(diags, moreDiags...)
	if diags.HasErrors() {
		return cfg.SourceFiles, diags
	}

	if len(plan.ToState) == 0 && len(plan.ToConfig) == 0 {
		fmt.Printf("Nothing to do! Everything in your terrafy configuration is already known to Terraform.\n\n")
		return cfg.SourceFiles, diags
	}

	plan.Sort()
	fmt.Printf("Import plan:\n")
	for _, planItem := range plan.ToState {
		fmt.Printf("- Create Terraform state binding from %s to remote object %q\n", planItem.Target, planItem.ID)
	}
	for _, planItem := range plan.ToConfig {
		fmt.Printf("- Generate a new %s configuration block in %s\n", planItem.Target, planItem.Filename)
	}

	fmt.Printf("\nDo you want to proceed? (Only \"yes\" will be accepted to confirm.)\n> ")
	termR := bufio.NewReader(os.Stdin)
	answer, err := termR.ReadString('\n')
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to read confirmation",
			Detail:   fmt.Sprintf("Error reading the confirmation response: %s.", err),
		})
		return cfg.SourceFiles, diags
	}
	answer = strings.TrimSpace(answer)
	if answer != "yes" {
		fmt.Printf("Cancelled.\n")
		return cfg.SourceFiles, diags
	}
	fmt.Println("")

	moreDiags = applyImporting(plan, tf)
	diags = append(diags, moreDiags...)

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

func planImporting(cfg *Config, idsRaw map[string]interface{}, tf *tfexec.Terraform) (*importPlan, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	state, err := tf.Show(context.Background())
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to read current state",
			Detail:   fmt.Sprintf("Could not read the latest state snapshot for this configuration:\n\n%s", err),
		})
		return nil, diags
	}
	var existing []*tfjson.StateResource
	if state.Values != nil && state.Values.RootModule != nil {
		existing = state.Values.RootModule.Resources
	}

	var importToState []*importPlanState
	var importToConfig []*importPlanConfig
	for addrStr, rawIds := range idsRaw {
		var imp *ImportConfig
		var addr resourceAddr
		for foundAddr, foundImp := range cfg.ImportConfigs {
			if foundAddr.String() == addrStr {
				imp = foundImp
				addr = foundAddr
			}
		}
		if imp == nil {
			// weird...
			continue
		}

		idsVal, err := prepareRawIDs(rawIds)
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid id value",
				Detail:   fmt.Sprintf("The id argument for %s is invalid: %s.", addrStr, err),
				Subject:  imp.ID.Range().Ptr(),
			})
			continue
		}

		instanceIDs := imp.Addr.InstanceIDs(idsVal)
	Instances:
		for instAddr, id := range instanceIDs {
			// If this instance is already in the state then we'll skip
			// trying to import it again, because that'd fail.
			for _, candidate := range existing {
				if candidate.Address == instAddr.String() {
					continue Instances
				}
			}

			importToState = append(importToState, &importPlanState{
				ID:     id,
				Target: instAddr,
			})
		}

		_, alreadyInConfig := cfg.ManagedResources[addr]
		if !alreadyInConfig {
			sourceFilename := imp.DefRange.Filename
			targetFilename := "imported.tf"
			if strings.HasSuffix(sourceFilename, ".tfy") {
				targetFilename = sourceFilename[:len(sourceFilename)-1]
			}

			importToConfig = append(importToConfig, &importPlanConfig{
				Target:   addr,
				Filename: targetFilename,
			})
		}
	}

	return &importPlan{
		ToState:  importToState,
		ToConfig: importToConfig,
	}, diags
}

func prepareRawIDs(raw interface{}) (cty.Value, error) {
	switch rv := raw.(type) {
	case []interface{}:
		norm := make([]cty.Value, len(rv))
		for i := range rv {
			s, err := prepareRawID(rv[i])
			if err != nil {
				return cty.NilVal, err
			}
			norm[i] = cty.StringVal(s)
		}
		return cty.ListVal(norm), nil
	case map[string]interface{}:
		norm := make(map[string]cty.Value, len(rv))
		for k := range rv {
			s, err := prepareRawID(rv[k])
			if err != nil {
				return cty.NilVal, err
			}
			norm[k] = cty.StringVal(s)
		}
		return cty.MapVal(norm), nil
	default:
		s, err := prepareRawID(raw)
		if err != nil {
			return cty.NilVal, err
		}
		return cty.StringVal(s), nil
	}
}

func applyImporting(plan *importPlan, tf *tfexec.Terraform) hcl.Diagnostics {
	var diags hcl.Diagnostics

	fmt.Printf("Importing:\n")
	for _, action := range plan.ToState {
		targetStr := action.Target.String()
		dispTargetStr := strings.Replace(targetStr, "'", "'\\''", 0)
		dispIDStr := strings.Replace(action.ID, "'", "'\\''", 0)
		fmt.Printf("- terraform import '%s' '%s'\n", dispTargetStr, dispIDStr)

		err := tf.Import(context.Background(), targetStr, action.ID, tfexec.AllowMissingConfig(true))
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Import failed",
				Detail:   fmt.Sprintf("Could not import %s with id %q:\n\n%s", targetStr, action.ID, err),
			})
			// We could potentially continue trying to import other objects
			// here, but we'll assume that the user would rather stop and
			// address whatever issue made this fail rather than potentially
			// see a series of repeated similar failures, if the problem is
			// a general one, such as the state storage server being
			//  unreachable.
			return diags
		}
	}

	for _, action := range plan.ToConfig {
		fmt.Printf("- adding a new resource %q %q block to %s\n", action.Target.Type, action.Target.Name, action.Filename)

		// If the target file already exists then we'll append a new block
		// to it, but if it doesn't exist then we'll just create a new file.
		var oldSrc []byte
		if src, err := ioutil.ReadFile(action.Filename); err == nil {
			oldSrc = src
		}

		f, moreDiags := hclwrite.ParseConfig(oldSrc, action.Filename, hcl.InitialPos)
		diags = append(diags, moreDiags...)
		if moreDiags.HasErrors() {
			// Something funny seems to be going on, because we presumably
			// managed to parse this same file earlier on using the main
			// hclsyntax parser.
			return diags
		}

		body := f.Body()
		body.AppendNewline()
		block := body.AppendNewBlock("resource", []string{action.Target.Type, action.Target.Name})
		// TODO: If the state shows this resource as belonging to a provider
		// configuration other than the one its type name seems to imply,
		// we'll need to generate a "provider = " declaration.
		block = block // TODO: continue to append stuff to this

		newSrc := f.Bytes()
		err := ioutil.WriteFile(action.Filename, newSrc, os.ModePerm)
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Failed to update configuration file",
				Detail:   fmt.Sprintf("Could not update %s with new configuration for %s: %s.", action.Filename, action.Target, err),
			})
			return diags
		}
	}

	if !diags.HasErrors() {
		fmt.Printf("\nAll done! Confirm the result by trying to create a Terraform plan:\n    terraform plan\n\n")
	}

	return diags
}

func prepareRawID(raw interface{}) (string, error) {
	switch rv := raw.(type) {
	case string:
		return rv, nil
	case float64:
		// Because ids are sometimes integers, we'll accept a number as an
		// attempt to use some decimal digits as an id.
		return strconv.FormatFloat(rv, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("must be a string, a list of strings, or a map of strings")
	}
}
