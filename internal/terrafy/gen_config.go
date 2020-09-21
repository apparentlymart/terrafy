package terrafy

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

func generateResourceConfig(addr resourceAddr, instances map[resourceInstanceAddr]*tfjson.StateResource, schema *tfjson.Schema, body *hclwrite.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// The data format in tfjson.StateResource is pretty inconvenient for our
	// purposes here because it's all raw output of encoding/json.Unmarshal,
	// but hclwrite wants to work with cty.Value.
	//
	// To make our life easier we'll convert over to cty.Value instead, though
	// we must do a bit of an abstraction inversion to get that done because
	// the typical way to get a cty.Value from a Terraform state is to parse
	// its JSON with cty's own JSON parser, using the schema's implied type.
	wantTy := schemaImpliedType(schema)
	instVals := map[resourceInstanceAddr]cty.Value{}
	for addr, state := range instances {
		inp := state.AttributeValues
		jsonSrc, err := json.Marshal(inp)
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid resource instance state data",
				Detail:   fmt.Sprintf("Resource instance %s has invalid state data: %s.", addr, err),
			})
			return diags
		}

		obj, err := ctyjson.Unmarshal(jsonSrc, wantTy)
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid resource instance state data",
				Detail:   fmt.Sprintf("Resource instance %s has invalid state data: %s.", addr, err),
			})
			return diags
		}

		instVals[addr] = obj
	}

	moreDiags := generateConfigBody(addr, nil, instVals, schema.Block, body)
	diags = append(diags, moreDiags...)
	return diags
}

func generateConfigBody(addr resourceAddr, path cty.Path, vals map[resourceInstanceAddr]cty.Value, schema *tfjson.SchemaBlock, body *hclwrite.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	attrNames := make([]string, 0, len(schema.Attributes))
	blockTypeNames := make([]string, 0, len(schema.NestedBlocks))
	for name := range schema.Attributes {
		attrNames = append(attrNames, name)
	}
	for typeName := range schema.NestedBlocks {
		blockTypeNames = append(blockTypeNames, typeName)
	}
	sort.Strings(attrNames)
	sort.Strings(blockTypeNames)

	// We'll reuse this map between iterations over our attributes because
	// we'll always have the same keys but we'll overwrite the values
	// each time.
	attrVals := make(map[resourceInstanceAddr]cty.Value, len(vals))

	for _, name := range attrNames {
		attrS := schema.Attributes[name]
		if !(attrS.Required || attrS.Optional) {
			// This attribute is not assignable in the configuration.
			continue
		}

		for instAddr, obj := range vals {
			if !obj.Type().HasAttribute(name) {
				// Weird, but we'll allow it to avoid crashing.
				attrVals[instAddr] = cty.NullVal(schemaAttrImpliedType(attrS))
			}
			attrVals[instAddr] = obj.GetAttr(name)
		}

		moreDiags := generateConfigAttribute(addr, path, name, attrVals, attrS, body)
		diags = append(diags, moreDiags...)
	}

	// TODO: Also generate the nested blocks, if any.

	return diags
}

func generateConfigAttribute(addr resourceAddr, containerPath cty.Path, name string, vals map[resourceInstanceAddr]cty.Value, schema *tfjson.SchemaAttribute, body *hclwrite.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// In the ideal case all of the values are equal and so we can just generate
	// a constant expression. However, we'll end up with this as cty.NilVal
	// if we find that the values are inconsistent between instances because
	// that'll mean we need to generate a dynamic selection expression instead.
	var singleVal cty.Value
	for _, v := range vals {
		if singleVal == cty.NilVal {
			singleVal = v
			continue
		}
		if !singleVal.RawEquals(v) {
			// We've found a difference, so we can't just use a constant.
			singleVal = cty.NilVal
			break
		}
	}

	if singleVal != cty.NilVal {
		// Easy case!
		if !singleVal.IsNull() {
			body.SetAttributeValue(name, singleVal)
		}
		return diags
	}

	// If we don't have the same value for all instances then we have to
	// generate a dynamic lookup based on the instance key. We don't have
	// enough information to generate the sort of dynamic lookup an end-user
	// would typically write, so we'll just generate a straightforward
	// (but very ugly) table lookup based on either count.index or each.key,
	// depending on which repetition mode this resource uses.

	// TODO: Actually implement that dynamic lookup. For now, this is just
	// a stub.
	body.SetAttributeValue(name, cty.NullVal(cty.DynamicPseudoType))

	return diags
}