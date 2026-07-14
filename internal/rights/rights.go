// Package rights is the trust boundary for a firm's intelligence exhaust — the
// prompts, context, traces and corrections that cross the wire when an agent uses
// somebody else's model. It answers one question, deterministically: given what a
// seller asked for and what a buyer consented to, what may actually cross, and
// what is that consent worth?
//
// The governing rule is deny-by-default. Nothing is granted that was not both
// asked for and explicitly consented to. Silence never grants
// (docs/08-learning-boundary.md).
package rights

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/tollgate/tollgate/x402"
)

// Right is one thing a seller may do with the exhaust of a call.
type Right string

const (
	// Retain stores the request/response beyond serving it.
	Retain Right = "retain"
	// Train trains or fine-tunes the seller's models on it — the right that turns
	// your corrections into their institutional know-how.
	Train Right = "train"
	// Distill uses the outputs to distill another model.
	Distill Right = "distill"
	// ShareThirdParty passes it to anyone else.
	ShareThirdParty Right = "share_third_party"
	// HumanReview shows it to a human reviewer.
	HumanReview Right = "human_review"
	// ImproveMemory folds it into cross-customer memory.
	ImproveMemory Right = "improve_memory"
)

// known is the closed vocabulary. Rights are a fixed, audited set — not a free
// text field — so a policy can reason about them and a receipt can prove them.
var known = map[Right]bool{
	Retain: true, Train: true, Distill: true,
	ShareThirdParty: true, HumanReview: true, ImproveMemory: true,
}

// Known returns the full vocabulary, sorted.
func Known() []Right {
	out := make([]Right, 0, len(known))
	for r := range known {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Valid reports whether r is a known right.
func Valid(r Right) bool { return known[r] }

// Offer is a seller's claim on the exhaust of a call. Required rights are
// non-negotiable — the seller will not serve the request without them. Optional
// rights are asked for, with a per-right Rebate (minor units) paid back to the
// buyer if granted: the price of learning from you.
type Offer struct {
	Required []Right         `json:"required,omitempty"`
	Optional []Right         `json:"optional,omitempty"`
	Rebates  map[Right]int64 `json:"rebates,omitempty"`
}

// Validate rejects malformed offers: unknown rights, negative rebates, or a
// rebate on a right that was never asked for.
func (o Offer) Validate() error {
	asked := make(map[Right]bool)
	for _, r := range append(append([]Right{}, o.Required...), o.Optional...) {
		if !Valid(r) {
			return fmt.Errorf("rights: unknown right %q", r)
		}
		asked[r] = true
	}
	for r, amt := range o.Rebates {
		if !Valid(r) {
			return fmt.Errorf("rights: rebate for unknown right %q", r)
		}
		if amt < 0 {
			return fmt.Errorf("rights: negative rebate %d for %q", amt, r)
		}
		if !asked[r] {
			return fmt.Errorf("rights: rebate offered for %q which is not asked for", r)
		}
	}
	return nil
}

// Asked is every right the offer puts on the table (required ∪ optional), sorted.
func (o Offer) Asked() []Right {
	return Canonical(append(append([]Right{}, o.Required...), o.Optional...))
}

// Effective is what may actually cross the boundary: the intersection of what was
// asked and what was granted. Anything granted but not asked for is dropped (a
// buyer cannot be talked into giving more than the seller requested), and anything
// asked for but not granted is refused. This is the deny-by-default rule.
func Effective(o Offer, granted []Right) []Right {
	askedSet := make(map[Right]bool)
	for _, r := range o.Asked() {
		askedSet[r] = true
	}
	var out []Right
	for _, g := range Canonical(granted) {
		if askedSet[g] {
			out = append(out, g)
		}
	}
	return out
}

// Satisfied reports whether the grant covers everything the seller requires. When
// it does not, the seller will not serve the call — the payment must be denied and
// nothing may cross.
func Satisfied(o Offer, granted []Right) bool {
	return len(Missing(o, granted)) == 0
}

// Missing lists the required rights the buyer did not grant — the exact reason a
// call was refused, for the audit log and the error message.
func Missing(o Offer, granted []Right) []Right {
	grantedSet := make(map[Right]bool)
	for _, g := range granted {
		grantedSet[g] = true
	}
	var missing []Right
	for _, r := range Canonical(o.Required) {
		if !grantedSet[r] {
			missing = append(missing, r)
		}
	}
	return missing
}

// Rebate is the data dividend: what the seller pays the buyer for the rights that
// actually crossed. Rights granted but never asked for earn nothing; a right with
// no advertised rebate is simply free to grant. The caller clamps this to the
// price — see Clamp.
func Rebate(o Offer, effective []Right) int64 {
	var total int64
	for _, r := range effective {
		total += o.Rebates[r]
	}
	return total
}

// Clamp holds a dividend inside [0, price]. A call can be made free by a generous
// rebate, but never negative: the rail settles payments, not reverse-payments.
func Clamp(rebate, price int64) int64 {
	if rebate < 0 {
		return 0
	}
	if rebate > price {
		return price
	}
	return rebate
}

// Canonical sorts and dedupes a right set so that two grants naming the same
// rights in a different order are the same grant.
func Canonical(rs []Right) []Right {
	if len(rs) == 0 {
		return nil
	}
	seen := make(map[Right]bool, len(rs))
	out := make([]Right, 0, len(rs))
	for _, r := range rs {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---- x402 wire conversion ----
//
// x402 is deliberately dependency-free and carries rights as plain strings; the
// vocabulary and semantics live here. These converters are the only seam.

// ToWire encodes an offer for the facilitator-signed quote.
func (o Offer) ToWire() *x402.ExhaustOffer {
	if len(o.Required) == 0 && len(o.Optional) == 0 {
		return nil // the seller claims nothing
	}
	w := &x402.ExhaustOffer{
		Required: Strings(Canonical(o.Required)),
		Optional: Strings(Canonical(o.Optional)),
	}
	if len(o.Rebates) > 0 {
		w.Rebates = make(map[string]string, len(o.Rebates))
		for r, amt := range o.Rebates {
			w.Rebates[string(r)] = strconv.FormatInt(amt, 10)
		}
	}
	return w
}

// FromWire decodes an offer carried in a quote. An unparseable rebate is treated
// as zero rather than an error: a malformed discount must never become a reason to
// hand over rights, and the buyer still gets a correct rights decision.
func FromWire(w *x402.ExhaustOffer) Offer {
	if w == nil {
		return Offer{}
	}
	o := Offer{
		Required: ParseRights(w.Required),
		Optional: ParseRights(w.Optional),
	}
	if len(w.Rebates) > 0 {
		o.Rebates = make(map[Right]int64, len(w.Rebates))
		for r, amt := range w.Rebates {
			n, err := strconv.ParseInt(amt, 10, 64)
			if err != nil || n < 0 {
				n = 0
			}
			o.Rebates[Right(r)] = n
		}
	}
	return o
}

// ParseRights converts wire strings to rights, dropping any the vocabulary does
// not know. An unknown right is not a grantable right.
func ParseRights(ss []string) []Right {
	var out []Right
	for _, s := range ss {
		if r := Right(s); Valid(r) {
			out = append(out, r)
		}
	}
	return Canonical(out)
}

// Strings converts rights to their wire form.
func Strings(rs []Right) []string {
	if len(rs) == 0 {
		return nil
	}
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}
