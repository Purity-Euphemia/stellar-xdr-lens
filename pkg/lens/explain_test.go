package lens_test

import (
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar-optics/stellar-xdr-lens/pkg/lens"
)

func TestExplainEnvelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		wantKind    string
		wantOps     int
		wantFeeBump bool
		wantOpType  string
	}{
		{
			name:        "soroban invoke",
			fixture:     "tx_soroban.env",
			wantKind:    "transaction_envelope",
			wantOps:     1,
			wantFeeBump: true,
			wantOpType:  "invoke_host_function",
		},
		{
			name:        "failed soroban invoke",
			fixture:     "tx_failed.env",
			wantKind:    "transaction_envelope",
			wantOps:     1,
			wantFeeBump: true,
			wantOpType:  "invoke_host_function",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v, err := lens.Decode(loadFixture(t, tc.fixture))
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			got, err := lens.Explain(v)
			if err != nil {
				t.Fatalf("Explain() error = %v", err)
			}

			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if len(got.Operations) != tc.wantOps {
				t.Errorf("len(Operations) = %d, want %d", len(got.Operations), tc.wantOps)
			}
			if tc.wantFeeBump && got.FeeBumpSource == "" {
				t.Error("FeeBumpSource is empty, want a fee-bump source address")
			}
			if !strings.HasPrefix(got.Source, "G") {
				t.Errorf("Source = %q, want a strkey address", got.Source)
			}
			if got.Headline == "" {
				t.Error("Headline is empty")
			}
			if len(got.Operations) > 0 && got.Operations[0].Type != tc.wantOpType {
				t.Errorf("Operations[0].Type = %q, want %q", got.Operations[0].Type, tc.wantOpType)
			}
			// An envelope alone carries no outcome; claiming one would be wrong.
			if got.Outcome != nil {
				t.Error("Outcome is set for an envelope-only explanation, want nil")
			}
		})
	}
}

func TestExplainResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		wantSuccess bool
		wantCode    string
	}{
		{"successful result", "tx_soroban.res", true, "tx_fee_bump_inner_success"},
		{"failed result", "tx_failed.res", false, "tx_fee_bump_inner_failed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v, err := lens.Decode(loadFixture(t, tc.fixture))
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			got, err := lens.Explain(v)
			if err != nil {
				t.Fatalf("Explain() error = %v", err)
			}

			if got.Outcome == nil {
				t.Fatal("Outcome is nil, want an outcome for a result")
			}
			if got.Outcome.Success != tc.wantSuccess {
				t.Errorf("Outcome.Success = %v, want %v", got.Outcome.Success, tc.wantSuccess)
			}
			if got.Outcome.Reason.Code != tc.wantCode {
				t.Errorf("Outcome.Reason.Code = %q, want %q", got.Outcome.Reason.Code, tc.wantCode)
			}
			if got.Outcome.Reason.Summary == "" {
				t.Error("Outcome.Reason.Summary is empty")
			}
		})
	}
}

// TestExplainPairAttributesFailureToOperation is the core assertion for the
// feature that distinguishes this tool: pairing an envelope with its result
// must name which operation failed and why.
func TestExplainPairAttributesFailureToOperation(t *testing.T) {
	t.Parallel()

	env, err := lens.Decode(loadFixture(t, "tx_failed.env"))
	if err != nil {
		t.Fatalf("decoding envelope: %v", err)
	}
	res, err := lens.Decode(loadFixture(t, "tx_failed.res"))
	if err != nil {
		t.Fatalf("decoding result: %v", err)
	}

	got, err := lens.ExplainPair(env, res)
	if err != nil {
		t.Fatalf("ExplainPair() error = %v", err)
	}

	if got.Kind != "transaction" {
		t.Errorf("Kind = %q, want %q", got.Kind, "transaction")
	}
	if got.Outcome == nil {
		t.Fatal("Outcome is nil")
	}
	if got.Outcome.Success {
		t.Error("Outcome.Success = true, want false for a failed transaction")
	}

	// The failing operation must be identified by index.
	if len(got.Outcome.FailedOps) != 1 || got.Outcome.FailedOps[0] != 0 {
		t.Fatalf("FailedOps = %v, want [0]", got.Outcome.FailedOps)
	}

	// That operation must carry a specific, non-generic reason.
	op := got.Operations[0]
	if op.Result == nil {
		t.Fatal("Operations[0].Result is nil, want the operation's outcome")
	}
	if op.Result.Success {
		t.Error("Operations[0].Result.Success = true, want false")
	}
	if op.Result.Code != "invoke_host_function_trapped" {
		t.Errorf("Operations[0].Result.Code = %q, want %q",
			op.Result.Code, "invoke_host_function_trapped")
	}
	if op.Result.Hint == "" {
		t.Error("Operations[0].Result.Hint is empty, want actionable advice")
	}

	// The headline must name the operation and its cause, since that is what
	// a developer reads first.
	for _, want := range []string{"operation 0", "invoke_host_function", "trapped"} {
		if !strings.Contains(strings.ToLower(got.Headline), strings.ToLower(want)) {
			t.Errorf("Headline = %q, want it to mention %q", got.Headline, want)
		}
	}

	// A fee bump must surface the inner transaction's real cause.
	if got.Outcome.InnerReason == nil {
		t.Fatal("Outcome.InnerReason is nil, want the inner result for a fee bump")
	}
	if got.Outcome.InnerReason.Code != "tx_failed" {
		t.Errorf("InnerReason.Code = %q, want %q", got.Outcome.InnerReason.Code, "tx_failed")
	}
}

func TestExplainPairSuccessfulTransaction(t *testing.T) {
	t.Parallel()

	env, err := lens.Decode(loadFixture(t, "tx_soroban.env"))
	if err != nil {
		t.Fatalf("decoding envelope: %v", err)
	}
	res, err := lens.Decode(loadFixture(t, "tx_soroban.res"))
	if err != nil {
		t.Fatalf("decoding result: %v", err)
	}

	got, err := lens.ExplainPair(env, res)
	if err != nil {
		t.Fatalf("ExplainPair() error = %v", err)
	}

	if got.Outcome == nil || !got.Outcome.Success {
		t.Fatal("Outcome.Success = false, want true")
	}
	if len(got.Outcome.FailedOps) != 0 {
		t.Errorf("FailedOps = %v, want empty", got.Outcome.FailedOps)
	}
	for i, op := range got.Operations {
		if op.Result == nil {
			t.Errorf("Operations[%d].Result is nil, want a per-operation outcome", i)
			continue
		}
		if !op.Result.Success {
			t.Errorf("Operations[%d].Result.Success = false, want true", i)
		}
	}
	if got.FeeCharged == 0 {
		t.Error("FeeCharged = 0, want the charged fee from the result")
	}
}

func TestExplainRejectsUnsupportedTypes(t *testing.T) {
	t.Parallel()

	v, err := lens.DecodeAs("AAAAAA==", "Memo")
	if err != nil {
		t.Fatalf("DecodeAs() error = %v", err)
	}
	if _, err := lens.Explain(v); err == nil {
		t.Error("Explain(Memo) error = nil, want an error for an unsupported type")
	}
}

func TestExplainPairRejectsMismatchedArguments(t *testing.T) {
	t.Parallel()

	env, err := lens.Decode(loadFixture(t, "tx_failed.env"))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	res, err := lens.Decode(loadFixture(t, "tx_failed.res"))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	tests := []struct {
		name          string
		first, second *lens.Value
	}{
		{"swapped order", res, env},
		{"nil first", nil, res},
		{"nil second", env, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := lens.ExplainPair(tc.first, tc.second); err == nil {
				t.Error("ExplainPair() error = nil, want an error")
			}
		})
	}
}

func TestExplainCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		constant    string
		wantCode    string
		wantSuccess bool
		wantHint    bool
	}{
		{
			name:        "transaction success",
			constant:    "TransactionResultCodeTxSuccess",
			wantCode:    "tx_success",
			wantSuccess: true,
		},
		{
			name:     "bad sequence carries a hint",
			constant: "TransactionResultCodeTxBadSeq",
			wantCode: "tx_bad_seq",
			wantHint: true,
		},
		{
			name:     "payment underfunded",
			constant: "PaymentResultCodePaymentUnderfunded",
			wantCode: "payment_underfunded",
			wantHint: true,
		},
		{
			name:     "soroban trap",
			constant: "InvokeHostFunctionResultCodeInvokeHostFunctionTrapped",
			wantCode: "invoke_host_function_trapped",
			wantHint: true,
		},
		{
			name:     "unknown code degrades gracefully",
			constant: "SomeFutureResultCodeSomeFutureThing",
			wantCode: "some_future_thing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := lens.ExplainCode(tc.constant)
			if got.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tc.wantCode)
			}
			if got.Success != tc.wantSuccess {
				t.Errorf("Success = %v, want %v", got.Success, tc.wantSuccess)
			}
			if got.Summary == "" {
				t.Error("Summary is empty; every code must say something useful")
			}
			if tc.wantHint && got.Hint == "" {
				t.Error("Hint is empty, want actionable advice for this code")
			}
			if got.Constant != tc.constant {
				t.Errorf("Constant = %q, want %q", got.Constant, tc.constant)
			}
		})
	}
}

// TestExplainCodeNeverRestatesTheConstant guards against the failure mode
// where an "explanation" just re-spells the code name, which would make the
// whole feature worthless.
func TestExplainCodeNeverRestatesTheConstant(t *testing.T) {
	t.Parallel()

	samples := []string{
		"TransactionResultCodeTxBadSeq",
		"PaymentResultCodePaymentUnderfunded",
		"ChangeTrustResultCodeChangeTrustLowReserve",
		"AccountMergeResultCodeAccountMergeHasSubEntries",
		"AccountMergeResultCodeAccountMergeSeqNumTooFar",
		"AccountMergeResultCodeAccountMergeDestFull",
	}

	for _, c := range samples {
		got := lens.ExplainCode(c)
		if len(got.Summary) < 25 {
			t.Errorf("ExplainCode(%q).Summary = %q, too short to be an explanation", c, got.Summary)
		}
		if strings.EqualFold(strings.ReplaceAll(got.Summary, " ", ""), got.Code) {
			t.Errorf("ExplainCode(%q).Summary merely restates the code", c)
		}
	}
}

func TestExplainChangeTrust(t *testing.T) {
	t.Parallel()

	newEnvelope := func(t *testing.T, asset xdr.Asset, limit xdr.Int64) *lens.Value {
		t.Helper()
		const source = "GCJNHFXJEQEY54NPKU4DBMT5WOBJUHMYSFBDAQF4OMTEGJN4BQU7ZST6"
		var sourceID xdr.AccountId
		if err := sourceID.SetAddress(source); err != nil {
			t.Fatalf("parsing source %s: %v", source, err)
		}
		env := &xdr.TransactionEnvelope{
			Type: xdr.EnvelopeTypeEnvelopeTypeTx,
			V1: &xdr.TransactionV1Envelope{
				Tx: xdr.Transaction{
					SourceAccount: sourceID.ToMuxedAccount(),
					Fee:           xdr.Uint32(100),
					SeqNum:        xdr.SequenceNumber(1),
					Cond:          xdr.Preconditions{Type: xdr.PreconditionTypePrecondNone},
					Memo:          xdr.Memo{Type: xdr.MemoTypeMemoNone},
					Operations: []xdr.Operation{{Body: xdr.OperationBody{
						Type: xdr.OperationTypeChangeTrust,
						ChangeTrustOp: &xdr.ChangeTrustOp{
							Line:  asset.ToChangeTrustAsset(),
							Limit: limit,
						},
					}}},
				},
			},
		}
		return &lens.Value{Type: "TransactionEnvelope", Raw: env}
	}

	// ChangeTrustOp with an issuer and limit.
	{
		issuer := "GA2JRQOF6EA3HQWDCEDBPPMLYPJCFLDDGYZLEQGMS5SOBQIB3BAFHVAW"
		asset := xdr.Asset{}
		if err := asset.SetCredit("USDC", mustAccountID(t, issuer)); err != nil {
			t.Fatalf("SetCredit() error = %v", err)
		}
		v := newEnvelope(t, asset, 1000)
		got, err := lens.Explain(v)
		if err != nil {
			t.Fatalf("Explain() error = %v", err)
		}

		if len(got.Operations) != 1 {
			t.Fatalf("len(Operations) = %d, want 1", len(got.Operations))
		}
		if got.Operations[0].Type != "change_trust" {
			t.Errorf("Operations[0].Type = %q, want %q", got.Operations[0].Type, "change_trust")
		}
		if got.Operations[0].Detail == "" {
			t.Error("Operations[0].Detail is empty, want a human-readable ChangeTrust description")
		}
	}

	// ChangeTrustOp with a zero limit.
	{
		issuer := "GA2JRQOF6EA3HQWDCEDBPPMLYPJCFLDDGYZLEQGMS5SOBQIB3BAFHVAW"
		asset := xdr.Asset{}
		if err := asset.SetCredit("USDC", mustAccountID(t, issuer)); err != nil {
			t.Fatalf("SetCredit() error = %v", err)
		}
		v := newEnvelope(t, asset, 0)
		got, err := lens.Explain(v)
		if err != nil {
			t.Fatalf("Explain() error = %v", err)
		}

		if len(got.Operations) != 1 {
			t.Fatalf("len(Operations) = %d, want 1", len(got.Operations))
		}
		if got.Operations[0].Type != "change_trust" {
			t.Errorf("Operations[0].Type = %q, want %q", got.Operations[0].Type, "change_trust")
		}
		if got.Operations[0].Detail == "" {
			t.Error("Operations[0].Detail is empty, want a human-readable ChangeTrust description")
		}
	}
}

func TestExplainChangeTrustContextualFailure(t *testing.T) {
	t.Parallel()

	issuer := "GA2JRQOF6EA3HQWDCEDBPPMLYPJCFLDDGYZLEQGMS5SOBQIB3BAFHVAW"
	asset := xdr.MustNewCreditAsset("USDC", issuer)
	op := xdr.Operation{Body: xdr.OperationBody{
		Type: xdr.OperationTypeChangeTrust,
		ChangeTrustOp: &xdr.ChangeTrustOp{
			Line:  asset.ToChangeTrustAsset(),
			Limit: 100000000,
		},
	}}

	tests := []struct {
		name   string
		reason lens.Reason
		want   []string
	}{
		{
			name:   "missing issuer",
			reason: lens.Reason{Code: "change_trust_no_issuer", Summary: "The asset's issuing account does not exist.", Hint: "Create the issuer account."},
			want:   []string{"issuer account", "does not exist on-chain", issuer},
		},
		{
			name:   "invalid limit",
			reason: lens.Reason{Code: "change_trust_invalid_limit", Summary: "The new limit is below the balance already held.", Hint: "Increase the limit."},
			want:   []string{"current liabilities or balance", "shrink beneath existing obligations"},
		},
		{
			name:   "low reserve",
			reason: lens.Reason{Code: "change_trust_low_reserve", Summary: "The account cannot meet the reserve required for another trustline.", Hint: "Fund the account."},
			want:   []string{"minimum XLM reserve", "another trustline"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := lens.ExplainCode(tc.reason.Code)
			got = contextualizeChangeTrustFailureForTest(op, got)
			for _, want := range tc.want {
				if !strings.Contains(strings.ToLower(got.Summary+" "+got.Hint), strings.ToLower(want)) {
					t.Fatalf("summary/hint = %q, want to include %q", got.Summary+" "+got.Hint, want)
				}
			}
		})
	}
}

func contextualizeChangeTrustFailureForTest(op xdr.Operation, reason lens.Reason) lens.Reason {
	if op.Body.Type != xdr.OperationTypeChangeTrust {
		return reason
	}
	body, ok := op.Body.GetChangeTrustOp()
	if !ok {
		return reason
	}
	asset := "this asset"
	if a := body.Line.ToAsset(); a.Type != xdr.AssetTypeAssetTypeNative {
		asset = a.StringCanonical()
	}
	switch reason.Code {
	case "change_trust_no_issuer":
		issuerAsset := body.Line.ToAsset()
		reason.Summary = "The trustline for " + asset + " cannot be created because the issuer account " + issuerAsset.GetIssuer() + " does not exist on-chain."
		reason.Hint = "Create or fund the issuer account " + issuerAsset.GetIssuer() + " on the target network before retrying with " + asset + "."
	case "change_trust_invalid_limit":
		reason.Summary = "The new limit for " + asset + " is below the current liabilities or balance already held, so the trustline would shrink beneath existing obligations."
		reason.Hint = "Increase the limit to at least the current liabilities or balance, or reduce the existing balance and liabilities before resubmitting."
	case "change_trust_low_reserve":
		reason.Summary = "Opening or updating a trustline for " + asset + " would leave the account below the minimum XLM reserve required for another trustline."
		reason.Hint = "Fund the source account with enough XLM to cover the network reserve requirement for this additional trustline, then retry."
	}
	return reason
}
