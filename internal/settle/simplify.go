package settle

import (
	"fmt"
	"math"
	"sort"
)

// Debt represents a single payment that needs to happen.
type Debt struct {
	FromID int64
	ToID   int64
	Amount float64
}

// Simplify takes net balances (positive = owed money, negative = owes money)
// and returns the minimum set of transactions to settle all debts.
//
// Algorithm: greedy matching of largest creditor with largest debtor.
// This is optimal for most real-world cases and runs in O(n log n).
func Simplify(netBalances map[int64]float64) []Debt {
	const epsilon = 0.01 // ignore sub-cent imbalances

	type entry struct {
		id      int64
		balance float64
	}

	var creditors []entry // positive balance (owed money)
	var debtors   []entry // negative balance (owes money)

	for id, bal := range netBalances {
		if bal > epsilon {
			creditors = append(creditors, entry{id, bal})
		} else if bal < -epsilon {
			debtors = append(debtors, entry{id, -bal}) // store as positive
		}
	}

	// Sort descending by amount for greedy matching
	sort.Slice(creditors, func(i, j int) bool {
		return creditors[i].balance > creditors[j].balance
	})
	sort.Slice(debtors, func(i, j int) bool {
		return debtors[i].balance > debtors[j].balance
	})

	var debts []Debt
	ci, di := 0, 0

	for ci < len(creditors) && di < len(debtors) {
		transfer := math.Round(math.Min(creditors[ci].balance, debtors[di].balance)*100) / 100

		debts = append(debts, Debt{
			FromID: debtors[di].id,
			ToID:   creditors[ci].id,
			Amount: transfer,
		})

		creditors[ci].balance -= transfer
		debtors[di].balance -= transfer

		if creditors[ci].balance < epsilon {
			ci++
		}
		if debtors[di].balance < epsilon {
			di++
		}
	}

	return debts
}

// FormatDebts produces a human-readable settlement summary.
// nameFunc maps member IDs to display names.
func FormatDebts(debts []Debt, nameFunc func(int64) string) string {
	if len(debts) == 0 {
		return "All settled up! No outstanding balances."
	}

	result := "💰 Outstanding balances:\n\n"
	for _, d := range debts {
		result += fmt.Sprintf("  %s owes %s $%.2f\n",
			nameFunc(d.FromID),
			nameFunc(d.ToID),
			d.Amount,
		)
	}
	return result
}
