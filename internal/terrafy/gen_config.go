package terrafy

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
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

	moreDiags := generateConfigBody(addr, instVals, schema.Block, body)
	diags = append(diags, moreDiags...)
	return diags
}

func generateConfigBody(addr resourceAddr, vals map[resourceInstanceAddr]cty.Value, schema *tfjson.SchemaBlock, body *hclwrite.Body) hcl.Diagnostics {
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

		moreDiags := generateConfigAttribute(addr, name, attrVals, attrS, body)
		diags = append(diags, moreDiags...)
	}

	for _, typeName := range blockTypeNames {
		nestedS := schema.NestedBlocks[typeName]
		switch nestedS.NestingMode {
		case tfjson.SchemaNestingModeSingle:
			for instAddr, obj := range vals {
				if !obj.Type().HasAttribute(typeName) {
					attrVals[instAddr] = cty.NullVal(schemaBlockImpliedType(nestedS.Block))
				}
				attrVals[instAddr] = obj.GetAttr(typeName)
			}
			moreDiags := generateConfigBlock(addr, typeName, nil, attrVals, nestedS, body)
			diags = append(diags, moreDiags...)
		case tfjson.SchemaNestingModeList, tfjson.SchemaNestingModeSet:
			attrValSlices := make(map[resourceInstanceAddr][]cty.Value, len(attrVals))
			maxLen := 0
			for instAddr, obj := range vals {
				if !obj.Type().HasAttribute(typeName) {
					attrValSlices[instAddr] = nil
				}
				attrValSlices[instAddr] = obj.GetAttr(typeName).AsValueSlice()
				if l := len(attrValSlices[instAddr]); l > maxLen {
					maxLen = l
				}
			}

			for i := 0; i < maxLen; i++ {
				for instAddr, objs := range attrValSlices {
					if len(objs) > i {
						attrVals[instAddr] = objs[i]
					} else {
						attrVals[instAddr] = cty.NullVal(schemaBlockImpliedType(nestedS.Block))
					}
				}
				moreDiags := generateConfigBlock(addr, typeName, nil, attrVals, nestedS, body)
				diags = append(diags, moreDiags...)
			}

		case tfjson.SchemaNestingModeMap:
			attrValMaps := make(map[resourceInstanceAddr]map[string]cty.Value, len(attrVals))
			allKeys := make(map[string]struct{})
			for instAddr, obj := range vals {
				if !obj.Type().HasAttribute(typeName) {
					attrValMaps[instAddr] = nil
				}
				attrValMaps[instAddr] = obj.GetAttr(typeName).AsValueMap()
				for k := range attrValMaps[instAddr] {
					allKeys[k] = struct{}{}
				}
			}
			allKeyNames := make([]string, 0, len(allKeys))
			for k := range allKeys {
				allKeyNames = append(allKeyNames, k)
			}
			sort.Strings(allKeyNames)

			for _, k := range allKeyNames {
				for instAddr, objs := range attrValMaps {
					attrVals[instAddr] = objs[k]
					if attrVals[instAddr] == cty.NilVal {
						attrVals[instAddr] = cty.NullVal(schemaBlockImpliedType(nestedS.Block))
					}
				}
				moreDiags := generateConfigBlock(addr, typeName, []string{k}, attrVals, nestedS, body)
				diags = append(diags, moreDiags...)
			}
		}
	}

	return diags
}

func generateConfigAttribute(addr resourceAddr, name string, vals map[resourceInstanceAddr]cty.Value, schema *tfjson.SchemaAttribute, body *hclwrite.Body) hcl.Diagnostics {
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
	var indexTraversal hcl.Traversal
	var brackets [2]*hclwrite.Token
	for k := range vals {
		switch k.InstanceKey.(type) {
		case int:
			indexTraversal = hcl.Traversal{
				hcl.TraverseRoot{Name: "count"},
				hcl.TraverseAttr{Name: "index"},
			}
			brackets[0] = &hclwrite.Token{
				Type:  hclsyntax.TokenOBrack,
				Bytes: []byte{'['},
			}
			brackets[1] = &hclwrite.Token{
				Type:  hclsyntax.TokenCBrack,
				Bytes: []byte{']'},
			}
		case string:
			indexTraversal = hcl.Traversal{
				hcl.TraverseRoot{Name: "each"},
				hcl.TraverseAttr{Name: "key"},
			}
			brackets[0] = &hclwrite.Token{
				Type:  hclsyntax.TokenOBrace,
				Bytes: []byte{'{'},
			}
			brackets[1] = &hclwrite.Token{
				Type:  hclsyntax.TokenCBrace,
				Bytes: []byte{'}'},
			}
		default:
			panic(fmt.Sprintf("unexpected instance key type %T", k.InstanceKey))
		}
		break
	}
	indexTokens := hclwrite.TokensForTraversal(indexTraversal)

	keys := make([]interface{}, 0, len(vals))
	vVals := make(map[interface{}]cty.Value, len(vals))
	for addr, v := range vals {
		keys = append(keys, addr.InstanceKey)
		vVals[addr.InstanceKey] = v
	}
	sort.Slice(keys, func(i, j int) bool {
		iInt, iIsInt := keys[i].(int)
		jInt, jIsInt := keys[j].(int)
		iStr, _ := keys[i].(string)
		jStr, _ := keys[j].(string)
		switch {
		case iIsInt != jIsInt:
			return iIsInt
		case iIsInt:
			return iInt < jInt
		default:
			return iStr < jStr
		}
	})

	var tokens hclwrite.Tokens
	tokens = append(tokens, brackets[0])
	tokens = append(tokens, &hclwrite.Token{
		Type:  hclsyntax.TokenNewline,
		Bytes: []byte{'\n'},
	})
	for _, key := range keys {
		val := vVals[key]
		if keyStr, isStr := key.(string); isStr {
			kToks := hclwrite.TokensForValue(cty.StringVal(keyStr))
			tokens = append(tokens, kToks...)
			tokens = append(tokens, &hclwrite.Token{
				Type:  hclsyntax.TokenEqual,
				Bytes: []byte{'='},
			})
		}

		vToks := hclwrite.TokensForValue(val)
		tokens = append(tokens, vToks...)
		tokens = append(tokens, &hclwrite.Token{
			Type:  hclsyntax.TokenComma,
			Bytes: []byte{','},
		})
		tokens = append(tokens, &hclwrite.Token{
			Type:  hclsyntax.TokenNewline,
			Bytes: []byte{'\n'},
		})
	}
	tokens = append(tokens, brackets[1])
	tokens = append(tokens, &hclwrite.Token{
		Type:  hclsyntax.TokenOBrack,
		Bytes: []byte{'['},
	})
	tokens = append(tokens, indexTokens...)
	tokens = append(tokens, &hclwrite.Token{
		Type:  hclsyntax.TokenCBrack,
		Bytes: []byte{']'},
	})

	body.SetAttributeRaw(name, tokens)

	return diags
}

func generateConfigBlock(addr resourceAddr, typeName string, labels []string, vals map[resourceInstanceAddr]cty.Value, schema *tfjson.SchemaBlockType, body *hclwrite.Body) hcl.Diagnostics {
	block := body.AppendNewBlock(typeName, labels)
	return generateConfigBody(addr, vals, schema.Block, block.Body())
}
