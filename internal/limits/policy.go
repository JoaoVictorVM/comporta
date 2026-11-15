package limits

import "github.com/JoaoVictorVM/comporta/internal/config"

// relaxable reports whether a policy may let an attempt through this violation.
// Only calendar budgets with a proven history qualify: rate, concurrency and
// hard caps protect capacity and the financial ceiling, and an unknown budget
// is a failure, not a budget legitimately exceeded.
func relaxable(v Violation) bool {
	return !v.hardCap && !v.Unknown && (v.Kind == KindBudget || v.Kind == KindReservation)
}

// firstMandatory returns the first violation no policy may relax, keeping the
// stable evaluation order.
func firstMandatory(d Decision) *Violation {
	for i := range d.Exceeded {
		if !relaxable(d.Exceeded[i]) {
			return &d.Exceeded[i]
		}
	}
	return nil
}

func firstRelaxable(d Decision) *Violation {
	for i := range d.Exceeded {
		if relaxable(d.Exceeded[i]) {
			v := d.Exceeded[i]
			return &v
		}
	}
	return nil
}

// refusal turns a decision into a block whose primary is the violation that
// really stops the attempt. A relaxed budget never hides a mandatory limit.
func refusal(d Decision, primary *Violation) Decision {
	d.Allowed = false
	if primary != nil {
		v := *primary
		d.Primary = &v
	}
	return d
}

// choice is the outcome of the policy before anything is reserved.
type choice struct {
	candidate Candidate
	selection Selection
	decision  Decision
	// unavailable: the policy needed a target that could not be prepared.
	unavailable bool
}

// choose applies the on_limit policy. evaluate has no effects; it is called
// under the manager lock, so the choice and the reservation see the same state.
func choose(p Policy, original Candidate, target *Candidate, evaluate func(Candidate) (Decision, error)) (choice, error) {
	first, err := evaluate(original)
	if err != nil {
		return choice{}, err
	}
	admit := func(c Candidate, outcome PolicyOutcome, trigger *Violation, d Decision) choice {
		d.Allowed = true
		d.Primary, d.Exceeded = nil, nil
		return choice{candidate: c, selection: Selection{Candidate: c.ID, Model: c.Model, Outcome: outcome, Trigger: trigger}, decision: d}
	}
	if first.Allowed {
		return admit(original, PolicyNone, nil, first), nil
	}
	trigger := firstRelaxable(first)
	blocked := func(d Decision, primary *Violation) choice {
		return choice{selection: Selection{Outcome: PolicyBlocked}, decision: refusal(d, primary)}
	}
	if trigger == nil {
		return blocked(first, first.Primary), nil
	}
	switch p.OnLimit {
	case config.OnLimitWarn:
		if mandatory := firstMandatory(first); mandatory != nil {
			return blocked(first, mandatory), nil
		}
		return admit(original, PolicyWarned, trigger, first), nil
	case config.OnLimitDowngrade:
		// A hard cap of the original alone may still let a cheaper target in;
		// every other mandatory limit stops the attempt whatever is sent.
		for i := range first.Exceeded {
			if v := first.Exceeded[i]; !relaxable(v) && !v.hardCap {
				return blocked(first, &v), nil
			}
		}
		if target == nil && original.Model == p.DowngradeTo {
			target = &original
		}
		if target == nil || target.Unavailable {
			c := blocked(first, trigger)
			c.unavailable = true
			return c, nil
		}
		second := first
		if target.ID != original.ID {
			if second, err = evaluate(*target); err != nil {
				return choice{}, err
			}
		}
		if mandatory := firstMandatory(second); mandatory != nil {
			return blocked(second, mandatory), nil
		}
		return admit(*target, PolicyDowngraded, trigger, second), nil
	}
	return blocked(first, first.Primary), nil
}
