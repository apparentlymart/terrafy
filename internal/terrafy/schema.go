package terrafy

import (
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/zclconf/go-cty/cty"
)

func schemaImpliedType(schema *tfjson.Schema) cty.Type {
	return schemaBlockImpliedType(schema.Block)
}

func schemaBlockImpliedType(blockS *tfjson.SchemaBlock) cty.Type {
	// The implied type of a block is always an object type.
	attrs := make(map[string]cty.Type)

	for name, attrS := range blockS.Attributes {
		attrs[name] = schemaAttrImpliedType(attrS)
	}

	for typeName, nestedS := range blockS.NestedBlocks {
		aty := schemaNestedBlockImpliedType(nestedS)
		if aty != cty.NilType {
			attrs[typeName] = aty
		}
	}

	return cty.Object(attrs)
}

func schemaAttrImpliedType(attrS *tfjson.SchemaAttribute) cty.Type {
	return attrS.AttributeType
}

func schemaNestedBlockImpliedType(nestedS *tfjson.SchemaBlockType) cty.Type {
	ety := schemaBlockImpliedType(nestedS.Block)

	switch nestedS.NestingMode {
	case tfjson.SchemaNestingModeSingle:
		return ety
	case tfjson.SchemaNestingModeList:
		return cty.List(ety)
	case tfjson.SchemaNestingModeMap:
		return cty.Map(ety)
	case tfjson.SchemaNestingModeSet:
		return cty.Set(ety)
	default:
		// Something new that we don't know about yet, presumably.
		// We'll just ignore it and hope for the best. (This might make
		// subsequent decoding fail.)
		return cty.NilType
	}
}
