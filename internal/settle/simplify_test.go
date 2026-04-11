package settle

import (
	"math"
	"testing"
)

func TestSimplify_AllSettled(t *testing.T) {
	balances := map[int64]float64{
		1: 0.0,
		2: 0.0,
		3: 0.0,
	}
	debts := Simplify(balances)
	if len(debts) != 0 {
		t.Errorf("expected 0 debts, got %d", len(debts))
	}
}

func TestSimplify_TwoPeople(t *testing.T) {
	// Alice paid $100 dinner split 2 ways. Alice is owed $50, Bob owes $50.
	balances := map[int64]float64{
		1: 50.0,  // Alice is owed
		2: -50.0, // Bob owes
	}
	debts := Simplify(balances)
	if len(debts) != 1 {
		t.Fatalf("expected 1 debt, got %d", len(debts))
	}
	if debts[0].FromID != 2 || debts[0].ToID != 1 || debts[0].Amount != 50.0 {
		t.Errorf("unexpected debt: %+v", debts[0])
	}
}

func TestSimplify_ThreePeopleChain(t *testing.T) {
	// A owes B $10, B owes C $10 → simplify to A owes C $10
	// Net: A = -10, B = 0, C = +10
	balances := map[int64]float64{
		1: -10.0, // A owes
		2: 0.0,   // B is settled
		3: 10.0,  // C is owed
	}
	debts := Simplify(balances)
	if len(debts) != 1 {
		t.Fatalf("expected 1 debt (simplified), got %d", len(debts))
	}
	if debts[0].FromID != 1 || debts[0].ToID != 3 {
		t.Errorf("expected A→C, got %d→%d", debts[0].FromID, debts[0].ToID)
	}
	if math.Abs(debts[0].Amount-10.0) > 0.01 {
		t.Errorf("expected $10, got $%.2f", debts[0].Amount)
	}
}

func TestSimplify_ComplexGroup(t *testing.T) {
	// 4-person group after several expenses:
	// Alice: +30 (owed money)
	// Bob:   -20 (owes)
	// Carol: +5  (owed)
	// Dave:  -15 (owes)
	balances := map[int64]float64{
		1: 30.0,
		2: -20.0,
		3: 5.0,
		4: -15.0,
	}
	debts := Simplify(balances)

	// Verify total transfers balance out
	var totalTransferred float64
	for _, d := range debts {
		totalTransferred += d.Amount
	}

	// Total debt should equal total credit ($35)
	if math.Abs(totalTransferred-35.0) > 0.01 {
		t.Errorf("expected $35 total transferred, got $%.2f", totalTransferred)
	}

	// Should need at most 3 transactions (n-1 where n=4 non-zero balances)
	if len(debts) > 3 {
		t.Errorf("expected at most 3 transactions, got %d", len(debts))
	}
}

func TestSimplify_SubCentIgnored(t *testing.T) {
	balances := map[int64]float64{
		1: 0.005,
		2: -0.005,
	}
	debts := Simplify(balances)
	if len(debts) != 0 {
		t.Errorf("sub-cent balances should be ignored, got %d debts", len(debts))
	}
}

func TestFormatDebts_Empty(t *testing.T) {
	result := FormatDebts(nil, func(id int64) string { return "x" })
	if result != "All settled up! No outstanding balances." {
		t.Errorf("unexpected format for empty debts: %s", result)
	}
}
