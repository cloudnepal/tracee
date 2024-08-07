package flags

import (
	"fmt"
	"strings"

	"github.com/aquasecurity/tracee/pkg/errfmt"
	"github.com/aquasecurity/tracee/pkg/events"
	"github.com/aquasecurity/tracee/pkg/filters"
	k8s "github.com/aquasecurity/tracee/pkg/k8s/apis/tracee.aquasec.com/v1beta1"
	"github.com/aquasecurity/tracee/pkg/policy"
)

// PrepareFilterMapsForPolicies prepares the scope and events PolicyFilterMap for the policies
func PrepareFilterMapsFromPolicies(policies []k8s.PolicyInterface) (PolicyScopeMap, PolicyEventMap, error) {
	policyScopeMap := make(PolicyScopeMap)
	policyEventsMap := make(PolicyEventMap)

	if len(policies) == 0 {
		return nil, nil, errfmt.Errorf("no policies provided")
	}

	if len(policies) > policy.PolicyMax {
		return nil, nil, errfmt.Errorf("too many policies provided, there is a limit of %d policies", policy.PolicyMax)
	}

	policyNames := make(map[string]bool)

	for pIdx, p := range policies {
		if _, ok := policyNames[p.GetName()]; ok {
			return nil, nil, errfmt.Errorf("policy %s already exist", p.GetName())
		}
		policyNames[p.GetName()] = true

		scopeFlags := make([]scopeFlag, 0)

		// scope
		for _, s := range p.GetScope() {
			s = strings.ReplaceAll(s, " ", "")

			if s == "global" && len(p.GetScope()) > 1 {
				return nil, nil, errfmt.Errorf("policy %s, global scope must be unique", p.GetName())
			}

			if s == "global" {
				break
			}

			parsed, err := parseScopeFlag(s)
			if err != nil {
				return nil, nil, errfmt.WrapError(err)
			}

			scopeFlags = append(scopeFlags, parsed)
		}

		policyScopeMap[pIdx] = policyScopes{
			policyName: p.GetName(),
			scopeFlags: scopeFlags,
		}

		eventFlags := make([]eventFlag, 0)

		for _, r := range p.GetRules() {
			evtFlags, err := parseEventFlag(r.Event)
			if err != nil {
				return nil, nil, errfmt.WrapError(err)
			}
			eventFlags = append(eventFlags, evtFlags...)

			for _, f := range r.Filters {
				// event data or return value filter
				// option "args." will be deprecate in future
				if strings.HasPrefix(f, "data.") || strings.HasPrefix(f, "args.") ||
					strings.HasPrefix(f, "retval") {
					evtFilterFlags, err := parseEventFlag(fmt.Sprintf("%s.%s", r.Event, f))
					if err != nil {
						return nil, nil, errfmt.WrapError(err)
					}
					eventFlags = append(eventFlags, evtFilterFlags...)

					continue
				}

				// at this point we know the filter is an event scope filter
				// scope filters are provided without "scope." prefix so we need to add it
				evtScopeFlags, err := parseEventFlag(fmt.Sprintf("%s.scope.%s", r.Event, f))
				if err != nil {
					return nil, nil, errfmt.WrapError(err)
				}
				eventFlags = append(eventFlags, evtScopeFlags...)
			}
		}

		policyEventsMap[pIdx] = policyEvents{
			policyName: p.GetName(),
			eventFlags: eventFlags,
		}
	}

	return policyScopeMap, policyEventsMap, nil
}

// CreatePolicies creates a Policies object from the scope and events maps.
func CreatePolicies(policyScopeMap PolicyScopeMap, policyEventsMap PolicyEventMap, newBinary bool) ([]*policy.Policy, error) {
	eventsNameToID := events.Core.NamesToIDs()
	// remove internal events since they shouldn't be accessible by users
	for event, id := range eventsNameToID {
		if events.Core.GetDefinitionByID(id).IsInternal() {
			delete(eventsNameToID, event)
		}
	}

	policies := make([]*policy.Policy, 0, len(policyScopeMap))
	for policyIdx, policyScopeFilters := range policyScopeMap {
		p := policy.NewPolicy()
		p.ID = policyIdx
		p.Name = policyScopeFilters.policyName

		for _, scopeFlag := range policyScopeFilters.scopeFlags {
			// The filters which are more common (container, event, pid, set, uid) can be given using a prefix of them.
			// Other filters should be given using their full name.
			// To avoid collisions between filters that share the same prefix, put the filters which should have an exact match first!
			if scopeFlag.scopeName == "comm" {
				err := p.CommFilter.Parse(scopeFlag.operatorAndValues)
				if err != nil {
					return nil, err
				}
				continue
			}

			if scopeFlag.scopeName == "exec" || scopeFlag.scopeName == "executable" ||
				scopeFlag.scopeName == "bin" || scopeFlag.scopeName == "binary" {
				// TODO: Rename BinaryFilter to ExecutableFilter
				err := p.BinaryFilter.Parse(scopeFlag.operatorAndValues)
				if err != nil {
					return nil, err
				}
				continue
			}

			if scopeFlag.scopeName == "container" {
				if scopeFlag.operator == "not" {
					err := p.ContFilter.Parse(scopeFlag.full)
					if err != nil {
						return nil, err
					}
					continue
				}
				if scopeFlag.operatorAndValues == "=new" {
					err := p.NewContFilter.Parse("new")
					if err != nil {
						return nil, err
					}
					continue
				}
				if scopeFlag.operatorAndValues == "!=new" {
					err := p.ContFilter.Parse(scopeFlag.scopeName)
					if err != nil {
						return nil, err
					}
					err = p.NewContFilter.Parse("!new")
					if err != nil {
						return nil, err
					}
					continue
				}
				if scopeFlag.operator == "=" {
					err := p.ContIDFilter.Parse(scopeFlag.operatorAndValues)
					if err != nil {
						return nil, err
					}
					continue
				}

				err := p.ContFilter.Parse(scopeFlag.scopeName)
				if err != nil {
					return nil, err
				}
				continue
			}

			if scopeFlag.scopeName == "mntns" {
				if strings.ContainsAny(scopeFlag.operator, "<>") {
					return nil, filters.InvalidExpression(scopeFlag.operatorAndValues)
				}
				err := p.MntNSFilter.Parse(scopeFlag.operatorAndValues)
				if err != nil {
					return nil, err
				}
				continue
			}

			if scopeFlag.scopeName == "pidns" {
				if strings.ContainsAny(scopeFlag.operator, "<>") {
					return nil, filters.InvalidExpression(scopeFlag.operatorAndValues)
				}
				err := p.PidNSFilter.Parse(scopeFlag.operatorAndValues)
				if err != nil {
					return nil, err
				}
				continue
			}

			if scopeFlag.scopeName == "tree" {
				err := p.ProcessTreeFilter.Parse(scopeFlag.operatorAndValues)
				if err != nil {
					return nil, err
				}
				continue
			}

			if scopeFlag.scopeName == "pid" {
				if scopeFlag.operatorAndValues == "=new" {
					if err := p.NewPidFilter.Parse("new"); err != nil {
						return nil, err
					}
					continue
				}
				if scopeFlag.operatorAndValues == "!=new" {
					if err := p.NewPidFilter.Parse("!new"); err != nil {
						return nil, err
					}
					continue
				}
				err := p.PIDFilter.Parse(scopeFlag.operatorAndValues)
				if err != nil {
					return nil, err
				}
				continue
			}

			if scopeFlag.scopeName == "uts" {
				err := p.UTSFilter.Parse(scopeFlag.operatorAndValues)
				if err != nil {
					return nil, err
				}
				continue
			}

			if scopeFlag.scopeName == "uid" {
				err := p.UIDFilter.Parse(scopeFlag.operatorAndValues)
				if err != nil {
					return nil, err
				}
				continue
			}

			if scopeFlag.scopeName == "follow" {
				p.Follow = true
				continue
			}

			return nil, InvalidScopeOptionError(scopeFlag.full, newBinary)
		}

		eventFilter := eventFilter{
			Equal:    []string{},
			NotEqual: []string{},
		}

		policyEvents, ok := policyEventsMap[policyIdx]
		if !ok {
			return nil, InvalidFlagEmpty()
		}

		for _, evtFlag := range policyEvents.eventFlags {
			if evtFlag.eventOptionType == "" {
				// no event option type means that the flag contains only event names
				if evtFlag.operator == "-" {
					eventFilter.NotEqual = append(eventFilter.NotEqual, evtFlag.eventName)
				} else {
					eventFilter.Equal = append(eventFilter.Equal, evtFlag.eventName)
				}
				continue
			}

			// at this point, we can assume that event flag is an event option filter (args, retval, scope),
			// so, as a sugar, we can add the event name to be filtered
			eventFilter.Equal = append(eventFilter.Equal, evtFlag.eventName)

			evtFilter := evtFlag.eventFilter
			operatorAndValues := evtFlag.operatorAndValues

			if evtFlag.eventOptionType == "retval" {
				err := p.RetFilter.Parse(evtFilter, operatorAndValues, eventsNameToID)
				if err != nil {
					return nil, err
				}
				continue
			}

			if evtFlag.eventOptionType == "scope" {
				err := p.ScopeFilter.Parse(evtFilter, operatorAndValues)
				if err != nil {
					return nil, err
				}
				continue
			}

			if evtFlag.eventOptionType == "data" || evtFlag.eventOptionType == "args" {
				err := p.DataFilter.Parse(evtFilter, operatorAndValues, eventsNameToID)
				if err != nil {
					return nil, err
				}
				continue
			}

			return nil, InvalidFilterFlagFormat(evtFlag.full)
		}

		var err error
		p.EventsToTrace, err = prepareEventsToTrace(eventFilter, eventsNameToID)
		if err != nil {
			return nil, err
		}

		policies = append(policies, p)
	}

	return policies, nil
}
