package terrafy

import "sort"

type importPlan struct {
	ToState  []*importPlanState
	ToConfig []*importPlanConfig
}

type importPlanState struct {
	Target resourceInstanceAddr
	ID     string
}

type importPlanConfig struct {
	Target     resourceAddr
	RepeatMode string // "", "count", or "for_each"
	Filename   string
}

func (p *importPlan) Sort() {
	sort.SliceStable(p.ToState, func(i, j int) bool {
		ai := p.ToState[i].Target
		aj := p.ToState[j].Target
		switch {
		case ai.Resource.Mode != aj.Resource.Mode:
			return ai.Resource.Mode < aj.Resource.Mode
		case ai.Resource.Type != aj.Resource.Type:
			return ai.Resource.Type < aj.Resource.Type
		case ai.Resource.Name != aj.Resource.Name:
			return ai.Resource.Name < aj.Resource.Name
		case ai.InstanceKey != aj.InstanceKey:
			kiStr, kiIsStr := ai.InstanceKey.(string)
			kjStr, kjIsStr := aj.InstanceKey.(string)
			kiInt, kiIsInt := ai.InstanceKey.(int)
			kjInt, kjIsInt := aj.InstanceKey.(int)
			if kiIsInt && kjIsStr {
				return true
			}
			if kiIsStr && kjIsInt {
				return false
			}
			if kiIsInt {
				return kiInt < kjInt
			}
			return kiStr < kjStr
		default:
			return false
		}
	})
	sort.SliceStable(p.ToConfig, func(i, j int) bool {
		ai := p.ToConfig[i].Target
		aj := p.ToConfig[j].Target
		switch {
		case ai.Mode != aj.Mode:
			return ai.Mode < aj.Mode
		case ai.Type != aj.Type:
			return ai.Type < aj.Type
		case ai.Name != aj.Name:
			return ai.Name < aj.Name
		default:
			return false
		}
	})
}
