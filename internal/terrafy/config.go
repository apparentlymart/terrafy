package terrafy

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/zclconf/go-cty/cty"
)

type resourceAddr struct {
	Mode tfjson.ResourceMode
	Type string
	Name string
}

func (addr resourceAddr) String() string {
	switch addr.Mode {
	case tfjson.ManagedResourceMode:
		return fmt.Sprintf("%s.%s", addr.Type, addr.Name)
	case tfjson.DataResourceMode:
		return fmt.Sprintf("data.%s.%s", addr.Type, addr.Name)
	default:
		panic("invalid resource address mode")
	}
}

func (addr resourceAddr) InstanceIDs(ids cty.Value) map[resourceInstanceAddr]string {
	switch {
	case ids.Type() == cty.String:
		return map[resourceInstanceAddr]string{
			{Resource: addr}: ids.AsString(),
		}
	case ids.Type().IsListType():
		l := ids.LengthInt()
		ret := make(map[resourceInstanceAddr]string, l)
		i := 0
		for it := ids.ElementIterator(); it.Next(); {
			_, v := it.Element()
			ret[resourceInstanceAddr{
				Resource:    addr,
				InstanceKey: i,
			}] = v.AsString()
			i++
		}
		return ret
	case ids.Type().IsMapType():
		l := ids.LengthInt()
		ret := make(map[resourceInstanceAddr]string, l)
		for it := ids.ElementIterator(); it.Next(); {
			k, v := it.Element()
			ret[resourceInstanceAddr{
				Resource:    addr,
				InstanceKey: k.AsString(),
			}] = v.AsString()
		}
		return ret
	default:
		panic("invalid instance ids value")
	}
}

type resourceInstanceAddr struct {
	Resource    resourceAddr
	InstanceKey interface{}
}

func (addr resourceInstanceAddr) String() string {
	switch k := addr.InstanceKey.(type) {
	case string:
		return fmt.Sprintf("%s[%q]", addr.Resource.String(), k)
	case int:
		return fmt.Sprintf("%s[%d]", addr.Resource.String(), k)
	case nil:
		return addr.Resource.String()
	default:
		panic(fmt.Sprintf("invalid resource instance key (%T)", k))
	}
}

type resourceAttr struct {
	Instance resourceInstanceAddr
	Name     string
}

type Config struct {
	ProviderReqs     map[string]hcl.Expression
	ProviderConfigs  map[string]*hcl.Block
	DataResources    map[resourceAddr]*hcl.Block
	ManagedResources map[resourceAddr]*hcl.Block
	ImportConfigs    map[resourceAddr]*ImportConfig

	SourceFiles map[string]*hcl.File
}

type ImportConfig struct {
	Addr resourceAddr
	ID   hcl.Expression

	DefRange hcl.Range
}

func LoadConfig(dir string) (*Config, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	ret := &Config{
		ProviderReqs:     map[string]hcl.Expression{},
		ProviderConfigs:  map[string]*hcl.Block{},
		DataResources:    map[resourceAddr]*hcl.Block{},
		ManagedResources: map[resourceAddr]*hcl.Block{},
		ImportConfigs:    map[resourceAddr]*ImportConfig{},
	}

	tfFiles, tfyFiles, err := findConfigFiles(dir)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to read configuration directory",
			Detail:   fmt.Sprintf("Failed to read files in %s: %s.", dir, err),
		})
		return ret, diags
	}

	parser := hclparse.NewParser()

	// The .tfy files are considered to be an extension of some things declared
	// in .tf files, and so we'll deal with the .tf files first here. We
	// use HCL's partial content mode for these, because we're knowingly only
	// processing a small subset of the Terraform language and don't want to
	// be constantly updating this to understand new Terraform language
	// features it doesn't use anyway.
	for _, fn := range tfFiles {
		var file *hcl.File
		if strings.HasSuffix(fn, ".json") {
			jsonFile, moreDiags := parser.ParseJSONFile(fn)
			diags = append(diags, moreDiags...)
			file = jsonFile
		} else {
			nativeFile, moreDiags := parser.ParseHCLFile(fn)
			diags = append(diags, moreDiags...)
			file = nativeFile
		}
		if file == nil {
			continue
		}

		body := file.Body
		content, _, moreDiags := body.PartialContent(tfSchema)
		diags = append(diags, moreDiags...)

		for _, block := range content.Blocks {
			switch block.Type {
			case "terraform":
				blockContent, _, moreDiags := block.Body.PartialContent(terraformBlockSchema)
				diags = append(diags, moreDiags...)
				for _, block := range blockContent.Blocks {
					attrs, moreDiags := block.Body.JustAttributes()
					diags = append(diags, moreDiags...)
					for name, attr := range attrs {
						if existing, exists := ret.ProviderReqs[name]; exists {
							diags = diags.Append(&hcl.Diagnostic{
								Severity: hcl.DiagError,
								Summary:  "Duplicate provider requirement",
								Detail:   fmt.Sprintf("A provider requirement with local name %q was already declared at %s.", name, existing.Range()),
								Subject:  attr.NameRange.Ptr(),
							})
							continue
						}
						ret.ProviderReqs[name] = attr.Expr
					}
				}

			case "provider":
				blockContent, _, moreDiags := block.Body.PartialContent(providerBlockSchema)
				diags = append(diags, moreDiags...)
				var alias string
				if attr, exists := blockContent.Attributes["alias"]; exists {
					moreDiags := gohcl.DecodeExpression(attr.Expr, nil, &alias)
					diags = append(diags, moreDiags...)
					if moreDiags.HasErrors() {
						continue
					}
				}
				key := block.Labels[0]
				if alias != "" {
					key = key + "." + alias
				}
				if existing, exists := ret.ProviderConfigs[key]; exists {
					diags = diags.Append(&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "Duplicate provider configuration",
						Detail:   fmt.Sprintf("A provider configuration %q was already declared at %s.", key, existing.DefRange),
						Subject:  block.DefRange.Ptr(),
					})
					continue
				}
				ret.ProviderConfigs[key] = block

			case "resource":
				// We track the managed resources in the configuration only
				// so that we can later skip generating new resource blocks
				// for the resources that are already declared.
				addr := resourceAddr{
					Mode: tfjson.ManagedResourceMode,
					Type: block.Labels[0],
					Name: block.Labels[1],
				}
				// TODO: Check that the labels are both valid identifiers.
				if existing, exists := ret.ManagedResources[addr]; exists {
					diags = diags.Append(&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "Duplicate managed resource configuration",
						Detail:   fmt.Sprintf("A managed resource %s was already defined at %s.", addr.String(), existing.DefRange),
						Subject:  block.DefRange.Ptr(),
					})
					continue
				}
				ret.ManagedResources[addr] = block

			default:
				panic("HCL produced a block type that wasn't in the schema")
			}
		}
	}

	// We are stricter about what's allowed in .tfy files because we want to
	// give better feedback if a user tries to use Terraform language features
	// rather than only features of the (much smaller) Terrafy language.
	for _, fn := range tfyFiles {
		file, moreDiags := parser.ParseHCLFile(fn)
		diags = append(diags, moreDiags...)
		if file == nil {
			continue
		}

		body := file.Body
		content, moreDiags := body.Content(tfySchema)
		diags = append(diags, moreDiags...)

		for _, block := range content.Blocks {
			switch block.Type {
			case "terraform":
				blockContent, moreDiags := block.Body.Content(terraformBlockSchema)
				diags = append(diags, moreDiags...)
				for _, block := range blockContent.Blocks {
					attrs, moreDiags := block.Body.JustAttributes()
					diags = append(diags, moreDiags...)
					for name, attr := range attrs {
						if existing, exists := ret.ProviderReqs[name]; exists {
							diags = diags.Append(&hcl.Diagnostic{
								Severity: hcl.DiagError,
								Summary:  "Duplicate provider requirement",
								Detail:   fmt.Sprintf("A provider requirement with local name %q was already declared at %s.", name, existing.Range()),
								Subject:  attr.NameRange.Ptr(),
							})
							continue
						}
						ret.ProviderReqs[name] = attr.Expr
					}
				}

			case "provider":
				blockContent, _, moreDiags := block.Body.PartialContent(providerBlockSchema)
				diags = append(diags, moreDiags...)
				var alias string
				if attr, exists := blockContent.Attributes["alias"]; exists {
					if strings.HasSuffix(attr.NameRange.Filename, ".tfy") {
						moreDiags := gohcl.DecodeExpression(attr.Expr, nil, &alias)
						diags = append(diags, moreDiags...)
						if moreDiags.HasErrors() {
							continue
						}
					}
				}
				key := block.Labels[0]
				if alias != "" {
					key = key + "." + alias
				}
				if existing, exists := ret.ProviderConfigs[key]; exists {
					diags = diags.Append(&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "Duplicate provider configuration",
						Detail:   fmt.Sprintf("A provider configuration %q was already defined at %s.", key, existing.DefRange),
						Subject:  block.DefRange.Ptr(),
					})
					continue
				}
				ret.ProviderConfigs[key] = block

			case "data":
				addr := resourceAddr{
					Mode: tfjson.DataResourceMode,
					Type: block.Labels[0],
					Name: block.Labels[1],
				}
				// TODO: Check that the labels are both valid identifiers.
				if existing, exists := ret.DataResources[addr]; exists {
					diags = diags.Append(&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "Duplicate data resource configuration",
						Detail:   fmt.Sprintf("A data resource %s was already defined at %s.", addr.String(), existing.DefRange),
						Subject:  block.DefRange.Ptr(),
					})
					continue
				}
				ret.DataResources[addr] = block

			case "import":
				addr := resourceAddr{
					Mode: tfjson.ManagedResourceMode,
					Type: block.Labels[0],
					Name: block.Labels[1],
				}
				// TODO: Check that the labels are both valid identifiers.
				if existing, exists := ret.ImportConfigs[addr]; exists {
					diags = diags.Append(&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "Duplicate data resource configuration",
						Detail:   fmt.Sprintf("An import configuration for %s was already defined at %s.", addr.String(), existing.DefRange),
						Subject:  block.DefRange.Ptr(),
					})
					continue
				}

				blockContent, moreDiags := block.Body.Content(importBlockSchema)
				diags = append(diags, moreDiags...)
				ret.ImportConfigs[addr] = &ImportConfig{
					Addr:     addr,
					ID:       blockContent.Attributes["id"].Expr,
					DefRange: block.DefRange,
				}

			default:
				panic("HCL produced a block type that wasn't in the schema")
			}
		}
	}

	ret.SourceFiles = parser.Files()

	return ret, diags
}

func findConfigFiles(dir string) (tfFiles, tfyFiles []string, err error) {
	candidates, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, info := range candidates {
		name := info.Name()
		if strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json") {
			tfFiles = append(tfFiles, filepath.Join(dir, name))
		}
		if strings.HasSuffix(name, ".tfy") {
			tfyFiles = append(tfyFiles, filepath.Join(dir, name))
		}
	}
	return tfFiles, tfyFiles, nil
}

var tfSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "terraform"},
		{Type: "provider", LabelNames: []string{"local_name"}},
		{Type: "resource", LabelNames: []string{"type", "name"}},
	},
}

var tfySchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "terraform"},
		{Type: "provider", LabelNames: []string{"local_name"}},
		{Type: "data", LabelNames: []string{"type", "name"}},
		{Type: "import", LabelNames: []string{"type", "name"}},
	},
}

var terraformBlockSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "required_providers"},
	},
}

var providerBlockSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "alias"},
	},
}

var importBlockSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "id", Required: true},
	},
}
