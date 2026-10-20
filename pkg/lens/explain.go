package lens

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/amount"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Summary is a human-readable account of an XDR value.
//
// A Summary is produced from an envelope, from a result, or from both
// together. Pairing the two is what makes the output genuinely useful: an
// envelope alone cannot say what failed, and a result alone cannot say what
// the failing operation was trying to do.
type Summary struct {
	// Kind is the sort of value summarised: "transaction_envelope",
	// "transaction_result", or "transaction" when the two were paired.
	Kind string `json:"kind"`
	// Headline is the single most important sentence, suitable for a log
	// line or a CI failure message.
	Headline string `json:"headline"`
	// Source is the strkey address that submitted the transaction.
	Source string `json:"source,omitempty"`
	// FeeBumpSource is set when the transaction was wrapped in a fee bump.
	FeeBumpSource string `json:"feeBumpSource,omitempty"`
	// Fee is the fee bid in stroops, and FeeCharged what was actually taken.
	Fee         int64    `json:"fee,omitempty"`
	FeeCharged  int64    `json:"feeCharged,omitempty"`
	SeqNum      int64    `json:"seqNum,omitempty"`
	Memo        string   `json:"memo,omitempty"`
	Preconds    []string `json:"preconditions,omitempty"`
	SignatureCt int      `json:"signatureCount,omitempty"`
	// Operations describes each operation, with its outcome when a result
	// was supplied.
	Operations []OpSummary `json:"operations,omitempty"`
	// Outcome is present whenever a result was supplied.
	Outcome *Outcome `json:"outcome,omitempty"`
}

// OpSummary describes one operation.
type OpSummary struct {
	// Index is the zero-based position in the transaction.
	Index int `json:"index"`
	// Type is the snake_case operation name, e.g. "payment".
	Type string `json:"type"`
	// Source is the operation-level source account override, when present.
	Source string `json:"source,omitempty"`
	// Detail is a one-line description of what the operation does.
	Detail string `json:"detail,omitempty"`
	// Result is the operation's outcome, present only when a result was
	// supplied and covers this operation.
	Result *Reason `json:"result,omitempty"`
}

// Outcome is the resolved result of a transaction.
type Outcome struct {
	// Success reports whether the transaction as a whole succeeded.
	Success bool `json:"success"`
	// Reason explains the transaction-level result code.
	Reason Reason `json:"reason"`
	// InnerReason is set for fee-bump transactions, and explains the result
	// of the inner transaction that the fee bump paid for.
	InnerReason *Reason `json:"innerReason,omitempty"`
	// FailedOps lists the indexes of operations that did not succeed.
	FailedOps []int `json:"failedOperations,omitempty"`
}

// Explain summarises a single decoded value.
//
// It accepts a TransactionEnvelope or a TransactionResult. For any other type
// it returns an error, since a general-purpose "explanation" of an arbitrary
// XDR struct would be no better than the decoded tree.
func Explain(v *Value) (*Summary, error) {
	if v == nil {
		return nil, fmt.Errorf("%w: nil value", ErrInvalidArgument)
	}
	switch raw := v.Raw.(type) {
	case *xdr.TransactionEnvelope:
		return explainEnvelope(raw, nil)
	case *xdr.TransactionResult:
		return explainResult(raw)
	default:
		return nil, fmt.Errorf("%w: cannot explain %s; explain supports TransactionEnvelope and TransactionResult", ErrUnsupportedType, v.Type)
	}
}

// ExplainPair summarises an envelope together with its result, attributing
// each result code to the operation it belongs to.
//
// This is the most useful form of explanation and the reason the command
// exists: it answers "which operation failed, what was it trying to do, and
// why did it fail" in one pass.
func ExplainPair(envelope, result *Value) (*Summary, error) {
	if envelope == nil || result == nil {
		return nil, fmt.Errorf("%w: both an envelope and a result are required", ErrInvalidArgument)
	}
	env, ok := envelope.Raw.(*xdr.TransactionEnvelope)
	if !ok {
		return nil, fmt.Errorf("%w: first value must be a TransactionEnvelope, got %s", ErrInvalidArgument, envelope.Type)
	}
	res, ok := result.Raw.(*xdr.TransactionResult)
	if !ok {
		return nil, fmt.Errorf("%w: second value must be a TransactionResult, got %s", ErrInvalidArgument, result.Type)
	}
	return explainEnvelope(env, res)
}

// explainEnvelope builds a Summary from an envelope, folding in a result when
// one is available.
func explainEnvelope(env *xdr.TransactionEnvelope, res *xdr.TransactionResult) (*Summary, error) {
	s := &Summary{
		Kind:        "transaction_envelope",
		Source:      addressOf(env.SourceAccount()),
		Fee:         int64(env.Fee()),
		SeqNum:      env.SeqNum(),
		Memo:        describeMemo(env.Memo()),
		SignatureCt: len(env.Signatures()),
	}
	if env.IsFeeBump() {
		s.FeeBumpSource = addressOf(env.FeeBumpAccount())
	}
	s.Preconds = describePreconditions(env)

	ops := env.Operations()
	for i, op := range ops {
		s.Operations = append(s.Operations, OpSummary{
			Index:  i,
			Type:   operationTypeLabel(op.Body.Type),
			Source: operationSource(op),
			Detail: describeOperation(op),
		})
	}

	if res == nil {
		s.Headline = envelopeHeadline(s)
		return s, nil
	}

	s.Kind = "transaction"
	if err := attachResult(s, res, ops, s.Source); err != nil {
		return nil, err
	}
	for i, op := range env.Operations() {
		if i >= len(s.Operations) || s.Operations[i].Result == nil {
			continue
		}
		reason := contextualizeChangeTrustFailure(op, *s.Operations[i].Result)
		s.Operations[i].Result = &reason
	}
	s.Headline = resultHeadline(s, s.Outcome)
	return s, nil
}

// explainResult builds a Summary from a result alone. Without the envelope
// there is nothing to say about what each operation was attempting, so only
// the outcome is populated.
func explainResult(res *xdr.TransactionResult) (*Summary, error) {
	s := &Summary{Kind: "transaction_result"}
	// No envelope means no operation bodies, so failures keep their generic
	// per-code explanation; deepenOperationFailure never runs.
	if err := attachResult(s, res, nil, ""); err != nil {
		return nil, err
	}
	return s, nil
}

// attachResult resolves a TransactionResult into an Outcome and pairs each
// operation result with its operation.
//
// ops carries the envelope's operations when one was supplied. They let a
// failed operation's explanation be deepened with the operation's own
// parameters (see deepenOperationFailure); without them the generic per-code
// reason stands.
func attachResult(s *Summary, res *xdr.TransactionResult, ops []xdr.Operation, source string) error {
	s.FeeCharged = int64(res.FeeCharged)

	code, err := resultCodeName(res)
	if err != nil {
		return err
	}
	outcome := &Outcome{
		Success: res.Successful(),
		Reason:  ExplainCode(code),
	}

	// A fee bump reports its own code plus the inner transaction's, and the
	// inner one is almost always the code the user actually cares about.
	if inner, ok := innerResultCodeName(res); ok {
		r := ExplainCode(inner)
		outcome.InnerReason = &r
	}

	opResults, ok := res.OperationResults()
	if ok {
		for i, opRes := range opResults {
			reason := explainOperationResult(opRes)
			if !reason.Success && i < len(ops) {
				reason = deepenOperationFailure(source, ops[i], reason)
			}
			if !reason.Success {
				outcome.FailedOps = append(outcome.FailedOps, i)
			}
			// Attach to the matching operation when the envelope was given.
			if i < len(s.Operations) {
				s.Operations[i].Result = &reason
			} else {
				// Result without a paired envelope: still surface it.
				s.Operations = append(s.Operations, OpSummary{
					Index:  i,
					Type:   "unknown",
					Result: &reason,
				})
			}
		}
	}

	s.Outcome = outcome
	s.Headline = resultHeadline(s, outcome)
	return nil
}

// explainOperationResult resolves an operation result to a Reason.
//
// The operation-level code says only whether the operation ran at all. When
// it did (opINNER), the meaningful code lives in the type-specific arm of the
// Tr union. Rather than switch over all 27 arms, the node tree is reused: it
// has already selected the populated arm and rendered its code as the
// constant name.
func explainOperationResult(opRes xdr.OperationResult) Reason {
	node := buildNode("", reflect.ValueOf(opRes), 0)
	if node == nil {
		return ExplainCode(opRes.Code.String())
	}

	// opINNER means "look inside"; any other code is itself the answer.
	if opRes.Code != xdr.OperationResultCodeOpInner {
		return ExplainCode(opRes.Code.String())
	}

	tr := node.Child("Tr")
	if tr == nil {
		return ExplainCode(opRes.Code.String())
	}
	for _, arm := range tr.Children {
		if arm.Name == "Type" {
			continue
		}
		if codeNode := arm.Child("Code"); codeNode != nil {
			if name, ok := codeNode.Value.(string); ok {
				return ExplainCode(name)
			}
		}
	}
	return ExplainCode(opRes.Code.String())
}

// ---------------------------------------------------------------------------
// Context-aware failure deepening
// ---------------------------------------------------------------------------
//
// explainOperationResult answers "what failed". When an envelope is paired
// with its result the operation bodies are also in hand, and for the
// operation families with the richest failure modes that is enough to say
// precisely why: which threshold exceeded 255, which flag bit is
// contradictory, how far the pool price drifted from the window the
// operation accepted.
//
// deepenOperationFailure refines such a Reason using the operation body. It
// only ever rewrites Summary and Hint for codes whose causes are provable
// from the operation's own parameters; Code and Constant are untouched, and
// anything it cannot pin down keeps the generic per-code explanation.
// Everything here is pure analysis of supplied XDR — no network calls, no
// ledger lookups — so output stays deterministic and offline-first.
func deepenOperationFailure(source string, op xdr.Operation, r Reason) Reason {
	if r.Success {
		return r
	}
	switch {
	case strings.HasPrefix(r.Constant, "SetOptionsResultCode"):
		if body, ok := op.Body.GetSetOptionsOp(); ok {
			return deepenSetOptionsFailure(operationEffectiveSource(source, op), body, r)
		}
	case strings.HasPrefix(r.Constant, "LiquidityPoolDepositResultCode"):
		if body, ok := op.Body.GetLiquidityPoolDepositOp(); ok {
			return deepenLiquidityPoolDepositFailure(body, r)
		}
	case strings.HasPrefix(r.Constant, "LiquidityPoolWithdrawResultCode"):
		if body, ok := op.Body.GetLiquidityPoolWithdrawOp(); ok {
			return deepenLiquidityPoolWithdrawFailure(body, r)
		}
	}
	return r
}

// operationEffectiveSource resolves the account an operation runs as: the
// operation-level override when present, otherwise the transaction source.
func operationEffectiveSource(txSource string, op xdr.Operation) string {
	if op.SourceAccount != nil {
		if addr, err := op.SourceAccount.GetAddress(); err == nil && addr != "" {
			return addr
		}
	}
	return txSource
}

// accountAuthFlagBits names the defined account-flag bits in bit order. The
// mask mirrors AUTH_FLAGS in stellar-core; anything outside it is rejected as
// an unknown flag.
var accountAuthFlagBits = [...]struct {
	bit  uint32
	name string
}{
	{0x1, "auth_required"},
	{0x2, "auth_revocable"},
	{0x4, "auth_immutable"},
	{0x8, "auth_clawback_enabled"},
}

const (
	authRequiredFlag        uint32 = 0x1
	authRevocableFlag       uint32 = 0x2
	authImmutableFlag       uint32 = 0x4
	authClawbackEnabledFlag uint32 = 0x8
	authFlagMask                   = authRequiredFlag | authRevocableFlag | authImmutableFlag | authClawbackEnabledFlag
)

// flagBitsNames renders set flag bits as "name (0xN)" pairs joined by
// "and", so a summary can name the exact bits involved in a failure.
func flagBitsNames(flags uint32) string {
	var parts []string
	for _, f := range accountAuthFlagBits {
		if flags&f.bit != 0 {
			parts = append(parts, fmt.Sprintf("%s (0x%x)", f.name, f.bit))
		}
	}
	if unknown := flags &^ authFlagMask; unknown != 0 {
		parts = append(parts, fmt.Sprintf("unrecognised bit(s) 0x%x", unknown))
	}
	return strings.Join(parts, " and ")
}

// deepenSetOptionsFailure refines a failed set_options explanation with the
// operation's own fields. stellar-core validates most of these conditions in
// doCheckValid, from the operation body alone, so every diagnosis below is
// provable offline from the XDR that was submitted.
func deepenSetOptionsFailure(source string, o xdr.SetOptionsOp, r Reason) Reason {
	switch r.Constant {
	case "SetOptionsResultCodeSetOptionsThresholdOutOfRange":
		// XDR carries thresholds as uint32 but the protocol only accepts
		// 0..255; the offending field is visible right here in the op.
		var over []string
		for _, f := range []struct {
			name string
			v    *xdr.Uint32
		}{
			{"master weight", o.MasterWeight},
			{"low threshold", o.LowThreshold},
			{"medium threshold", o.MedThreshold},
			{"high threshold", o.HighThreshold},
		} {
			if f.v != nil && uint64(*f.v) > 255 {
				over = append(over, fmt.Sprintf("%s %d", f.name, *f.v))
			}
		}
		if len(over) > 0 {
			r.Summary = fmt.Sprintf("%s is outside the allowed range 0–255 for threshold weights",
				strings.Join(over, ", "))
			r.Hint = "Threshold weights are a single byte; clamp each value to 0–255 before signing."
		}

	case "SetOptionsResultCodeSetOptionsBadFlags":
		// Core rejects an operation that sets and clears the same bit.
		if o.SetFlags != nil && o.ClearFlags != nil {
			if overlap := uint32(*o.SetFlags) & uint32(*o.ClearFlags); overlap != 0 {
				r.Summary = fmt.Sprintf(
					"the operation sets and clears the same account flag(s) in one change: %s; a flag cannot move in both directions at once",
					flagBitsNames(overlap))
				r.Hint = "Keep each flag on exactly one side of the operation, or split the change into two operations."
			}
		}

	case "SetOptionsResultCodeSetOptionsUnknownFlag":
		var unknown uint32
		if o.SetFlags != nil {
			unknown |= uint32(*o.SetFlags) &^ authFlagMask
		}
		if o.ClearFlags != nil {
			unknown |= uint32(*o.ClearFlags) &^ authFlagMask
		}
		if unknown != 0 {
			r.Summary = fmt.Sprintf(
				"the flags carry unrecognised bit(s) 0x%x; the only defined account flags are %s",
				unknown, flagBitsNames(authFlagMask))
			r.Hint = "Mask the flag value down to the four defined bits before submitting."
		}

	case "SetOptionsResultCodeSetOptionsCantChange":
		// Core returns this only when auth flags are touched on an account
		// whose auth-immutable flag is set, and that flag is permanent.
		var touched uint32
		if o.ClearFlags != nil {
			touched |= uint32(*o.ClearFlags)
			if uint32(*o.ClearFlags)&authImmutableFlag != 0 {
				r.Summary = "the operation tries to clear the auth-immutable flag, but that flag is permanent once set; no operation can clear it"
				r.Hint = "An immutable account's auth flags are locked forever. Use a different account."
				return r
			}
		}
		if o.SetFlags != nil {
			touched |= uint32(*o.SetFlags)
		}
		if touched &= authFlagMask; touched != 0 {
			r.Summary = fmt.Sprintf(
				"the operation changes account auth flags (%s), but the account has auth-immutable set, which permanently locks its flags",
				flagBitsNames(touched))
			r.Hint = "Nothing can change an immutable account's auth flags. Use a different account, or merge into a fresh one."
		}

	case "SetOptionsResultCodeSetOptionsLowReserve":
		// The only reserve a set_options op can add is a new signer subentry.
		if o.Signer != nil && o.Signer.Weight != 0 {
			r.Summary = fmt.Sprintf(
				"adding %s as a new signer subentry raises the account's reserve by one base reserve (0.5 XLM on Mainnet), and the remaining balance cannot cover it",
				signerKeyLabel(o.Signer.Key))
			r.Hint = "Fund the account by at least one more base reserve, or free reserve by removing an offer, trustline, data entry or signer first."
		}

	case "SetOptionsResultCodeSetOptionsTooManySigners":
		// Only a genuinely new signer key hits the cap; updating the weight
		// of an existing signer does not.
		if o.Signer != nil && o.Signer.Weight != 0 {
			r.Summary = fmt.Sprintf(
				"the account already holds the maximum of five signers and %s is not one of them; updating an existing signer's weight would not hit this limit",
				signerKeyLabel(o.Signer.Key))
			r.Hint = "Remove a signer first (the same key with weight 0) before adding a new one."
		}

	case "SetOptionsResultCodeSetOptionsBadSigner":
		if o.Signer == nil {
			break
		}
		if addr, err := o.Signer.Key.GetAddress(); err == nil && addr != "" && addr == source {
			r.Summary = fmt.Sprintf(
				"the operation adds the source account %s as a signer of itself; an account can never sign for its own address",
				shortAddr(addr))
			r.Hint = "Use a distinct key (or a pre-auth transaction hash) as the signer."
			return r
		}
		if o.Signer.Weight > 255 {
			r.Summary = fmt.Sprintf("signer weight %d is outside the allowed range 0–255", o.Signer.Weight)
			r.Hint = "Signer weights are a single byte; clamp the weight to 0–255."
		}

	case "SetOptionsResultCodeSetOptionsInvalidHomeDomain":
		if o.HomeDomain != nil {
			r.Summary = fmt.Sprintf("home domain %q is not a syntactically valid domain name", string(*o.HomeDomain))
			r.Hint = "Use a valid DNS-style name, or the empty string to clear the domain."
		}

	case "SetOptionsResultCodeSetOptionsAuthRevocableRequired":
		// Clawback is only legal on a revocable account.
		if o.SetFlags != nil && uint32(*o.SetFlags)&authClawbackEnabledFlag != 0 {
			r.Summary = "the operation enables auth-clawback on an account that is not auth-revocable; clawback is only allowed on revocable accounts"
			r.Hint = "Include auth_revocable (0x2) in the same change, or set it before enabling clawback."
		}
	}
	return r
}

// signerKeyLabel renders a signer key short enough for prose while keeping
// its kind recognisable. It never panics: nil union arms degrade to the kind
// alone.
func signerKeyLabel(k xdr.SignerKey) string {
	hexOf := func(b *xdr.Uint256) string {
		if b == nil {
			return ""
		}
		return " " + hex.EncodeToString(b[:])[:8] + "…"
	}
	switch k.Type {
	case xdr.SignerKeyTypeSignerKeyTypeEd25519:
		if k.Ed25519 == nil {
			return "signer"
		}
		if addr, err := (&k).GetAddress(); err == nil && addr != "" {
			return "signer " + shortAddr(addr)
		}
		return "signer ed25519" + hexOf(k.Ed25519)
	case xdr.SignerKeyTypeSignerKeyTypePreAuthTx:
		return "pre-auth signer" + hexOf(k.PreAuthTx)
	case xdr.SignerKeyTypeSignerKeyTypeHashX:
		return "hash-x signer" + hexOf(k.HashX)
	case xdr.SignerKeyTypeSignerKeyTypeEd25519SignedPayload:
		if k.Ed25519SignedPayload == nil {
			return "signed-payload signer"
		}
		if addr, err := (&k).GetAddress(); err == nil && addr != "" {
			return "signed-payload signer " + shortAddr(addr)
		}
		return "signed-payload signer"
	}
	return "signer"
}

// deepenLiquidityPoolDepositFailure refines a failed liquidity-pool-deposit
// explanation with the operation's own amounts and price bounds, following
// the checks in stellar-core's LiquidityPoolDepositOpFrame.
func deepenLiquidityPoolDepositFailure(o xdr.LiquidityPoolDepositOp, r Reason) Reason {
	switch r.Constant {
	case "LiquidityPoolDepositResultCodeLiquidityPoolDepositMalformed":
		if summary, ok := liquidityPoolDepositMalformedReason(o); ok {
			r.Summary = summary
			r.Hint = "Fix the deposit parameters: positive max amounts and a strictly positive, non-empty price window (minPrice ≤ maxPrice)."
		}

	case "LiquidityPoolDepositResultCodeLiquidityPoolDepositBadPrice":
		if o.MinPrice.N <= 0 || o.MinPrice.D <= 0 || o.MaxPrice.N <= 0 || o.MaxPrice.D <= 0 {
			break // degenerate bounds; the malformed diagnosis owns this case
		}
		r.Summary = fmt.Sprintf(
			"the pool's ratio at apply time fell outside the price window the deposit accepted (A:B between %s and %s) — slippage between quoting and submission",
			priceRatio(o.MinPrice), priceRatio(o.MaxPrice))
		hint := "Widen minPrice/maxPrice to tolerate pool movement (for example ±1–5% around the quoted ratio) and resubmit; core compares the actual deposit amounts, which follow the pool's current ratio, against the window."
		if o.MinPrice.N == o.MaxPrice.N && o.MinPrice.D == o.MaxPrice.D {
			hint = "The window pins one exact ratio (min == max), so any trade before inclusion breaks it. Widen minPrice/maxPrice around the quoted ratio (for example ±1–5%) and resubmit."
		}
		if dir := depositAmountsVsWindow(o); dir != 0 {
			which := "below"
			if dir > 0 {
				which = "above"
			}
			r.Summary += fmt.Sprintf(
				"; the offered amounts themselves (%s and %s) imply a ratio that already falls %s the window",
				amount.String(o.MaxAmountA), amount.String(o.MaxAmountB), which)
		}
		r.Hint = hint

	case "LiquidityPoolDepositResultCodeLiquidityPoolDepositUnderfunded":
		r.Summary = fmt.Sprintf(
			"the source cannot cover the deposit of %s and %s: the spendable balance of one asset (balance, minus the base reserve and any sell liabilities) is short",
			amount.String(o.MaxAmountA), amount.String(o.MaxAmountB))
		r.Hint = "Check both trustline balances; for XLM the spendable amount excludes the minimum reserve and open-offer liabilities."

	case "LiquidityPoolDepositResultCodeLiquidityPoolDepositPoolFull":
		r.Summary = fmt.Sprintf(
			"the deposit would push a reserve of pool %s, or the pool's total shares, past the protocol maximum (2^63−1); the pool has effectively reached its cap",
			poolIDLabel(o.LiquidityPoolId))
		r.Hint = "No smaller deposit fixes a pool at its cap; use a different pool, or withdraw first."

	case "LiquidityPoolDepositResultCodeLiquidityPoolDepositLineFull":
		r.Summary = fmt.Sprintf(
			"the deposit would mint pool shares for pool %s beyond the source's pool-share trustline limit",
			poolIDLabel(o.LiquidityPoolId))
		r.Hint = "Raise the trustline limit with change_trust (the maximum is INT64_MAX), or hold fewer pool shares."

	case "LiquidityPoolDepositResultCodeLiquidityPoolDepositNoTrust":
		r.Summary = fmt.Sprintf(
			"the source has no pool-share trustline for pool %s, so it cannot hold the shares this deposit mints",
			poolIDLabel(o.LiquidityPoolId))
		r.Hint = "Open the pool-share trustline with change_trust on that pool before depositing."

	case "LiquidityPoolDepositResultCodeLiquidityPoolDepositNotAuthorized":
		r.Summary = fmt.Sprintf(
			"one of the two asset trustlines behind pool %s is not authorised to hold its asset; the deposit cannot proceed until the issuer authorises it",
			poolIDLabel(o.LiquidityPoolId))
		r.Hint = "Ask the asset issuer to authorise the trustline (allow-trust or set-trustline-flags), then retry."

	case "LiquidityPoolDepositResultCodeLiquidityPoolDepositTrustlineFrozen":
		r.Summary = fmt.Sprintf(
			"one of the trustlines this deposit touches for pool %s is frozen; frozen balances cannot move into the pool",
			poolIDLabel(o.LiquidityPoolId))
		r.Hint = "Ask the asset issuer to unfreeze the trustline, then retry."
	}
	return r
}

// liquidityPoolDepositMalformedReason mirrors the doCheckValid rules of
// stellar-core's LiquidityPoolDepositOpFrame and names the first rule the
// operation breaks.
func liquidityPoolDepositMalformedReason(o xdr.LiquidityPoolDepositOp) (string, bool) {
	switch {
	case o.MaxAmountA <= 0:
		return fmt.Sprintf("maxAmountA is %s; a deposit must offer a positive amount of each asset",
			amount.String(o.MaxAmountA)), true
	case o.MaxAmountB <= 0:
		return fmt.Sprintf("maxAmountB is %s; a deposit must offer a positive amount of each asset",
			amount.String(o.MaxAmountB)), true
	case o.MinPrice.N <= 0 || o.MinPrice.D <= 0:
		return fmt.Sprintf("minPrice %s is not strictly positive on both sides",
			priceRatio(o.MinPrice)), true
	case o.MaxPrice.N <= 0 || o.MaxPrice.D <= 0:
		return fmt.Sprintf("maxPrice %s is not strictly positive on both sides",
			priceRatio(o.MaxPrice)), true
	case int64(o.MinPrice.N)*int64(o.MaxPrice.D) > int64(o.MinPrice.D)*int64(o.MaxPrice.N):
		return fmt.Sprintf("minPrice %s exceeds maxPrice %s, leaving an empty price window",
			priceRatio(o.MinPrice), priceRatio(o.MaxPrice)), true
	}
	return "", false
}

// depositAmountsVsWindow reports where the deposit's own maximum amounts sit
// relative to the accepted price window: -1 below it, +1 above, 0 inside.
// Exact integer arithmetic avoids the rounding that would make a boundary
// case lie.
func depositAmountsVsWindow(o xdr.LiquidityPoolDepositOp) int {
	a := new(big.Int).SetInt64(int64(o.MaxAmountA))
	b := new(big.Int).SetInt64(int64(o.MaxAmountB))
	minN, minD := big.NewInt(int64(o.MinPrice.N)), big.NewInt(int64(o.MinPrice.D))
	maxN, maxD := big.NewInt(int64(o.MaxPrice.N)), big.NewInt(int64(o.MaxPrice.D))
	// ratio is B per A: below the window when b/a < minN/minD, above it
	// when b/a > maxN/maxD.
	if b.Mul(b, minD).Cmp(a.Mul(a, minN)) < 0 {
		return -1
	}
	if b.Mul(b, maxD).Cmp(a.Mul(a, maxN)) > 0 {
		return 1
	}
	return 0
}

// deepenLiquidityPoolWithdrawFailure refines a failed
// liquidity-pool-withdraw explanation with the operation's own share amount
// and minimum-return bounds, following stellar-core's
// LiquidityPoolWithdrawOpFrame.
func deepenLiquidityPoolWithdrawFailure(o xdr.LiquidityPoolWithdrawOp, r Reason) Reason {
	switch r.Constant {
	case "LiquidityPoolWithdrawResultCodeLiquidityPoolWithdrawMalformed":
		switch {
		case o.Amount <= 0:
			r.Summary = fmt.Sprintf("the operation burns %s pool shares; the share amount must be positive",
				amount.String(o.Amount))
			r.Hint = "Set amount to the number of pool shares to burn, above zero."
		case o.MinAmountA < 0:
			r.Summary = fmt.Sprintf("minAmountA is %s; minimum returns cannot be negative",
				amount.String(o.MinAmountA))
			r.Hint = "Set each minimum return to zero or more."
		case o.MinAmountB < 0:
			r.Summary = fmt.Sprintf("minAmountB is %s; minimum returns cannot be negative",
				amount.String(o.MinAmountB))
			r.Hint = "Set each minimum return to zero or more."
		}

	case "LiquidityPoolWithdrawResultCodeLiquidityPoolWithdrawUnderfunded":
		r.Summary = fmt.Sprintf(
			"the account holds fewer than the %s of pool shares this withdrawal burns; open offers against the pool count as liabilities",
			amount.String(o.Amount))
		r.Hint = "Burn fewer shares (lower amount), or wait for offers that lock the shares to fill or be removed."

	case "LiquidityPoolWithdrawResultCodeLiquidityPoolWithdrawUnderMinimum":
		r.Summary = fmt.Sprintf(
			"burning %s pool shares would return less than the minimums demanded (at least %s and %s); the pool's ratio moved since the quote — slippage",
			amount.String(o.Amount), amount.String(o.MinAmountA), amount.String(o.MinAmountB))
		r.Hint = "Relax minAmountA/minAmountB to tolerate the pool's current ratio and retry; like a price limit, tight minimums fail whenever anyone trades the pool first."

	case "LiquidityPoolWithdrawResultCodeLiquidityPoolWithdrawLineFull":
		r.Summary = fmt.Sprintf(
			"the %s and %s this withdrawal returns would push one of the account's asset trustlines past its limit",
			amount.String(o.MinAmountA), amount.String(o.MinAmountB))
		r.Hint = "Lower the trustline limit or free up capacity on the asset the account already holds, then retry."

	case "LiquidityPoolWithdrawResultCodeLiquidityPoolWithdrawNoTrust":
		r.Summary = fmt.Sprintf(
			"the source has no pool-share trustline for pool %s, so it holds no shares to burn",
			poolIDLabel(o.LiquidityPoolId))
		r.Hint = "The account must hold pool shares before it can withdraw; check the pool id."

	case "LiquidityPoolWithdrawResultCodeLiquidityPoolWithdrawTrustlineFrozen":
		r.Summary = fmt.Sprintf(
			"one of the trustlines this withdrawal touches for pool %s is frozen; frozen balances cannot receive the returned assets",
			poolIDLabel(o.LiquidityPoolId))
		r.Hint = "Ask the asset issuer to unfreeze the trustline, then retry."
	}
	return r
}

// priceRatio renders an XDR price as the ratio developers quote it by.
func priceRatio(p xdr.Price) string {
	return fmt.Sprintf("%d/%d", p.N, p.D)
}

// poolIDLabel abbreviates a 32-byte pool id for inline prose, keeping enough
// of both ends to be recognisable.
func poolIDLabel(id xdr.PoolId) string {
	h := hex.EncodeToString(id[:])
	return h[:8] + "…" + h[len(h)-4:]
}

// resultCodeName returns the transaction-level result code constant name.
func resultCodeName(res *xdr.TransactionResult) (string, error) {
	name := safeString(res.Result.Code)
	if name == "" {
		return "", fmt.Errorf("%w: could not read transaction result code", ErrInvalidXDR)
	}
	return name, nil
}

// innerResultCodeName returns the inner transaction's result code for a
// fee-bump result, if this is one.
func innerResultCodeName(res *xdr.TransactionResult) (string, bool) {
	pair, ok := res.Result.GetInnerResultPair()
	if !ok {
		return "", false
	}
	name := safeString(pair.Result.Result.Code)
	if name == "" {
		return "", false
	}
	return name, true
}

func envelopeHeadline(s *Summary) string {
	var b strings.Builder
	b.WriteString("Transaction from ")
	b.WriteString(shortAddr(s.Source))
	fmt.Fprintf(&b, " with %d operation", len(s.Operations))
	if len(s.Operations) != 1 {
		b.WriteString("s")
	}
	if len(s.Operations) > 0 {
		types := make([]string, 0, len(s.Operations))
		seen := map[string]bool{}
		for _, op := range s.Operations {
			if !seen[op.Type] {
				seen[op.Type] = true
				types = append(types, op.Type)
			}
		}
		b.WriteString(" (" + strings.Join(types, ", ") + ")")
	}
	if s.FeeBumpSource != "" {
		b.WriteString(", fee-bumped by " + shortAddr(s.FeeBumpSource))
	}
	b.WriteString(".")
	return b.String()
}

func resultHeadline(s *Summary, o *Outcome) string {
	if o.Success {
		if len(s.Operations) > 0 && s.Kind == "transaction" {
			return fmt.Sprintf("Transaction succeeded: all %d operation(s) applied.", len(s.Operations))
		}
		return "Transaction succeeded."
	}

	// Prefer naming the operation that actually failed, which is the thing
	// the user is looking for.
	if len(o.FailedOps) > 0 {
		idx := o.FailedOps[0]
		if idx < len(s.Operations) {
			op := s.Operations[idx]
			label := op.Type
			if label == "" || label == "unknown" {
				label = "operation"
			}
			detail := ""
			if op.Detail != "" {
				detail = " (" + op.Detail + ")"
			}
			reason := ""
			if op.Result != nil {
				reason = " " + op.Result.Summary
			}
			return fmt.Sprintf("Transaction failed at operation %d, %s%s:%s", idx, label, detail, reason)
		}
	}

	if o.InnerReason != nil && !o.InnerReason.Success {
		return "Transaction failed: " + o.InnerReason.Summary
	}
	return "Transaction failed: " + o.Reason.Summary
}

// describeOperation renders a one-line description of what an operation does.
//
// The common operations get a purpose-written line, since those are what
// developers are usually debugging. Anything else falls back to its type
// name, which is still accurate — just less specific.
func describeOperation(op xdr.Operation) string {
	body := op.Body
	switch body.Type {
	case xdr.OperationTypeCreateAccount:
		o, ok := body.GetCreateAccountOp()
		if !ok {
			break
		}
		return fmt.Sprintf("create account %s funded with %s XLM",
			shortAddr(o.Destination.Address()), amount.String(o.StartingBalance))

	case xdr.OperationTypePayment:
		o, ok := body.GetPaymentOp()
		if !ok {
			break
		}
		return fmt.Sprintf("pay %s %s to %s",
			amount.String(o.Amount), assetLabel(o.Asset), shortAddr(addressOf(o.Destination)))

	case xdr.OperationTypePathPaymentStrictReceive:
		o, ok := body.GetPathPaymentStrictReceiveOp()
		if !ok {
			break
		}
		return fmt.Sprintf("send at most %s %s to deliver exactly %s %s to %s",
			amount.String(o.SendMax), assetLabel(o.SendAsset),
			amount.String(o.DestAmount), assetLabel(o.DestAsset), shortAddr(addressOf(o.Destination)))

	case xdr.OperationTypePathPaymentStrictSend:
		o, ok := body.GetPathPaymentStrictSendOp()
		if !ok {
			break
		}
		return fmt.Sprintf("send exactly %s %s to deliver at least %s %s to %s",
			amount.String(o.SendAmount), assetLabel(o.SendAsset),
			amount.String(o.DestMin), assetLabel(o.DestAsset), shortAddr(addressOf(o.Destination)))

	case xdr.OperationTypeManageSellOffer:
		o, ok := body.GetManageSellOfferOp()
		if !ok {
			break
		}
		return fmt.Sprintf("sell %s %s for %s at %d/%d (offer id %d)",
			amount.String(o.Amount), assetLabel(o.Selling), assetLabel(o.Buying),
			o.Price.N, o.Price.D, o.OfferId)

	case xdr.OperationTypeManageBuyOffer:
		o, ok := body.GetManageBuyOfferOp()
		if !ok {
			break
		}
		return fmt.Sprintf("buy %s %s with %s at %d/%d (offer id %d)",
			amount.String(o.BuyAmount), assetLabel(o.Buying), assetLabel(o.Selling),
			o.Price.N, o.Price.D, o.OfferId)

	case xdr.OperationTypeCreatePassiveSellOffer:
		o, ok := body.GetCreatePassiveSellOfferOp()
		if !ok {
			break
		}
		return fmt.Sprintf("passively sell %s %s for %s at %d/%d",
			amount.String(o.Amount), assetLabel(o.Selling), assetLabel(o.Buying), o.Price.N, o.Price.D)

	case xdr.OperationTypeChangeTrust:
		o, ok := body.GetChangeTrustOp()
		if !ok {
			break
		}
		if o.Limit == 0 {
			return fmt.Sprintf("remove trustline for %s", changeTrustAssetLabel(o.Line))
		}
		return fmt.Sprintf("trust %s up to %s", changeTrustAssetLabel(o.Line), amount.String(o.Limit))

	case xdr.OperationTypeAccountMerge:
		dest, ok := body.GetDestination()
		if !ok {
			break
		}
		// AccountMerge diagnostic guidance:
		// - account_merge_has_sub_entries: close all trustlines, DEX offers, and data entries before merging
		// - account_merge_seqnum_too_far: use a recent sequence number
		// - account_merge_dest_full: reduce the destination's balance or remove entries before merging
		return "merge this account into " + shortAddr(addressOf(dest))

	case xdr.OperationTypeBumpSequence:
		o, ok := body.GetBumpSequenceOp()
		if !ok {
			break
		}
		return fmt.Sprintf("bump sequence number to %d", int64(o.BumpTo))

	case xdr.OperationTypeManageData:
		o, ok := body.GetManageDataOp()
		if !ok {
			break
		}
		if o.DataValue == nil {
			return fmt.Sprintf("delete data entry %q", string(o.DataName))
		}
		return fmt.Sprintf("set data entry %q (%d bytes)", string(o.DataName), len(*o.DataValue))

	case xdr.OperationTypeSetOptions:
		o, ok := body.GetSetOptionsOp()
		if !ok {
			break
		}
		return describeSetOptions(o)

	case xdr.OperationTypeInvokeHostFunction:
		o, ok := body.GetInvokeHostFunctionOp()
		if !ok {
			break
		}
		return describeHostFunction(o)

	case xdr.OperationTypeExtendFootprintTtl:
		o, ok := body.GetExtendFootprintTtlOp()
		if !ok {
			break
		}
		return fmt.Sprintf("extend ledger entry lifetimes to %d ledgers", o.ExtendTo)

	case xdr.OperationTypeRestoreFootprint:
		return "restore archived ledger entries"

	case xdr.OperationTypeLiquidityPoolDeposit:
		o, ok := body.GetLiquidityPoolDepositOp()
		if !ok {
			break
		}
		return fmt.Sprintf("deposit up to %s and %s into pool %s within price bounds %s..%s",
			amount.String(o.MaxAmountA), amount.String(o.MaxAmountB),
			poolIDLabel(o.LiquidityPoolId), priceRatio(o.MinPrice), priceRatio(o.MaxPrice))

	case xdr.OperationTypeLiquidityPoolWithdraw:
		o, ok := body.GetLiquidityPoolWithdrawOp()
		if !ok {
			break
		}
		return fmt.Sprintf("burn %s pool shares of pool %s for at least %s and %s",
			amount.String(o.Amount), poolIDLabel(o.LiquidityPoolId),
			amount.String(o.MinAmountA), amount.String(o.MinAmountB))
	}
	return operationTypeLabel(body.Type)
}

func describeSetOptions(o xdr.SetOptionsOp) string {
	var parts []string
	if o.InflationDest != nil {
		parts = append(parts, "set inflation destination")
	}
	if o.ClearFlags != nil {
		parts = append(parts, "clear flags "+strconv.FormatUint(uint64(*o.ClearFlags), 10))
	}
	if o.SetFlags != nil {
		parts = append(parts, "set flags "+strconv.FormatUint(uint64(*o.SetFlags), 10))
	}
	if o.MasterWeight != nil {
		parts = append(parts, "master weight "+strconv.FormatUint(uint64(*o.MasterWeight), 10))
	}
	if o.LowThreshold != nil {
		parts = append(parts, "low threshold "+strconv.FormatUint(uint64(*o.LowThreshold), 10))
	}
	if o.MedThreshold != nil {
		parts = append(parts, "medium threshold "+strconv.FormatUint(uint64(*o.MedThreshold), 10))
	}
	if o.HighThreshold != nil {
		parts = append(parts, "high threshold "+strconv.FormatUint(uint64(*o.HighThreshold), 10))
	}
	if o.HomeDomain != nil {
		parts = append(parts, "home domain "+string(*o.HomeDomain))
	}
	if o.Signer != nil {
		parts = append(parts, fmt.Sprintf("signer weight %d", o.Signer.Weight))
	}
	if len(parts) == 0 {
		return "set options (no changes)"
	}
	return strings.Join(parts, ", ")
}

// describeHostFunction names the Soroban contract and function being called,
// which is the part a developer recognises.
func describeHostFunction(o xdr.InvokeHostFunctionOp) string {
	switch o.HostFunction.Type {
	case xdr.HostFunctionTypeHostFunctionTypeInvokeContract:
		inv, ok := o.HostFunction.GetInvokeContract()
		if !ok {
			break
		}
		contract := ""
		if addr, err := inv.ContractAddress.String(); err == nil {
			contract = shortAddr(addr)
		}
		fn := string(inv.FunctionName)
		return fmt.Sprintf("call %s on contract %s with %d argument(s)", fn, contract, len(inv.Args))

	case xdr.HostFunctionTypeHostFunctionTypeCreateContract,
		xdr.HostFunctionTypeHostFunctionTypeCreateContractV2:
		return "create a contract instance"

	case xdr.HostFunctionTypeHostFunctionTypeUploadContractWasm:
		wasm, ok := o.HostFunction.GetWasm()
		if !ok {
			break
		}
		return fmt.Sprintf("upload contract wasm (%d bytes)", len(wasm))
	}
	return "invoke host function"
}

// describePreconditions renders the transaction's preconditions as readable
// lines, omitting the ones that are unset.
func describePreconditions(env *xdr.TransactionEnvelope) []string {
	var out []string
	if tb := env.TimeBounds(); tb != nil {
		switch {
		case tb.MinTime == 0 && tb.MaxTime == 0:
			// No effective bound; say nothing rather than print zeros.
		case tb.MaxTime == 0:
			out = append(out, "valid from "+formatTime(int64(tb.MinTime)))
		case tb.MinTime == 0:
			out = append(out, "valid until "+formatTime(int64(tb.MaxTime)))
		default:
			out = append(out, "valid from "+formatTime(int64(tb.MinTime))+" until "+formatTime(int64(tb.MaxTime)))
		}
	}
	if lb := env.LedgerBounds(); lb != nil && (lb.MinLedger != 0 || lb.MaxLedger != 0) {
		out = append(out, fmt.Sprintf("ledger bounds %d..%d", lb.MinLedger, lb.MaxLedger))
	}
	if ms := env.MinSeqNum(); ms != nil {
		out = append(out, fmt.Sprintf("minimum sequence number %d", *ms))
	}
	if age := env.MinSeqAge(); age != nil && *age != 0 {
		out = append(out, fmt.Sprintf("minimum sequence age %ds", uint64(*age)))
	}
	if gap := env.MinSeqLedgerGap(); gap != nil && *gap != 0 {
		out = append(out, fmt.Sprintf("minimum sequence ledger gap %d", uint32(*gap)))
	}
	if extra := env.ExtraSigners(); len(extra) > 0 {
		out = append(out, fmt.Sprintf("%d extra signer(s) required", len(extra)))
	}
	return out
}

func describeMemo(m xdr.Memo) string {
	switch m.Type {
	case xdr.MemoTypeMemoNone:
		return ""
	case xdr.MemoTypeMemoText:
		if t, ok := m.GetText(); ok {
			return "text: " + t
		}
	case xdr.MemoTypeMemoId:
		if id, ok := m.GetId(); ok {
			return "id: " + strconv.FormatUint(uint64(id), 10)
		}
	case xdr.MemoTypeMemoHash:
		if h, ok := m.GetHash(); ok {
			return "hash: " + h.HexString()
		}
	case xdr.MemoTypeMemoReturn:
		if h, ok := m.GetRetHash(); ok {
			return "return: " + h.HexString()
		}
	}
	return strings.TrimPrefix(m.Type.String(), "MemoTypeMemo")
}

// operationSource returns the operation-level source account override, or ""
// when the operation inherits the transaction's source.
func operationSource(op xdr.Operation) string {
	if op.SourceAccount == nil {
		return ""
	}
	return addressOf(*op.SourceAccount)
}

func assetLabel(a xdr.Asset) string {
	if a.Type == xdr.AssetTypeAssetTypeNative {
		return "XLM"
	}
	return a.StringCanonical()
}

func changeTrustAssetLabel(a xdr.ChangeTrustAsset) string {
	if a.Type == xdr.AssetTypeAssetTypePoolShare {
		return "liquidity pool shares"
	}
	return assetLabel(a.ToAsset())
}

// addressOf renders a muxed account as strkey, tolerating a malformed value.
func addressOf(m xdr.MuxedAccount) string {
	addr, err := (&m).GetAddress()
	if err != nil {
		return ""
	}
	return addr
}

// shortAddr abbreviates a strkey address for inline prose, keeping enough of
// both ends to be recognisable.
func shortAddr(addr string) string {
	if len(addr) <= 12 {
		return addr
	}
	return addr[:5] + "…" + addr[len(addr)-4:]
}

func formatTime(unix int64) string {
	if unix == 0 {
		return "0"
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

func contextualizeChangeTrustFailure(op xdr.Operation, reason Reason) Reason {
	if op.Body.Type != xdr.OperationTypeChangeTrust {
		return reason
	}
	body, ok := op.Body.GetChangeTrustOp()
	if !ok {
		return reason
	}
	asset := changeTrustAssetLabel(body.Line)
	if asset == "" {
		asset = "this asset"
	}

	switch reason.Code {
	case "change_trust_malformed":
		reason.Summary = fmt.Sprintf("The ChangeTrust request for %s is malformed: the asset or limit is invalid.", asset)
		reason.Hint = fmt.Sprintf("Check the asset and limit for %s before resubmitting; a valid ChangeTrust operation must identify a valid asset and a non-negative limit.", asset)
	case "change_trust_no_issuer":
		issuer := changeTrustIssuerLabel(body.Line)
		if issuer == "" {
			reason.Summary = fmt.Sprintf("The trustline for %s cannot be created because the issuing account does not exist on-chain.", asset)
			reason.Hint = fmt.Sprintf("Create or fund the issuing account for %s on the target network, or switch to the correct network before retrying.", asset)
			return reason
		}
		reason.Summary = fmt.Sprintf("The trustline for %s cannot be created because the issuer account %s does not exist on-chain.", asset, issuer)
		reason.Hint = fmt.Sprintf("Create or fund the issuer account %s on the target network before retrying with %s.", issuer, asset)
	case "change_trust_invalid_limit":
		reason.Summary = fmt.Sprintf("The new limit for %s is below the current liabilities or balance already held, so the trustline would shrink beneath existing obligations.", asset)
		reason.Hint = "Increase the limit to at least the current liabilities or balance, or reduce the existing balance and liabilities before resubmitting."
	case "change_trust_low_reserve":
		reason.Summary = fmt.Sprintf("Opening or updating a trustline for %s would leave the account below the minimum XLM reserve required for another trustline.", asset)
		reason.Hint = "Fund the source account with enough XLM to cover the network reserve requirement for this additional trustline, then retry."
	}
	return reason
}

func changeTrustIssuerLabel(line xdr.ChangeTrustAsset) string {
	if line.Type == xdr.AssetTypeAssetTypeNative || line.Type == xdr.AssetTypeAssetTypePoolShare {
		return ""
	}
	asset := line.ToAsset()
	if asset.Type == xdr.AssetTypeAssetTypeNative {
		return ""
	}
	return asset.GetIssuer()
}
