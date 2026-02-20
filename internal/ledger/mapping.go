package ledger

import ledgerv1 "cryptoledger/gen/ledger/v1"

// В БД храним строковые коды, а не числа из proto: дамп читается человеком,
// а перенумерация enum в контракте не ломает историю.

var dbToKind = map[string]ledgerv1.AccountKind{
	"player_main":   ledgerv1.AccountKind_ACCOUNT_KIND_PLAYER_MAIN,
	"player_locked": ledgerv1.AccountKind_ACCOUNT_KIND_PLAYER_LOCKED,
	"player_bonus":  ledgerv1.AccountKind_ACCOUNT_KIND_PLAYER_BONUS,
	"house_hot":     ledgerv1.AccountKind_ACCOUNT_KIND_HOUSE_HOT,
	"house_equity":  ledgerv1.AccountKind_ACCOUNT_KIND_HOUSE_EQUITY,
	"fee":           ledgerv1.AccountKind_ACCOUNT_KIND_FEE,
}

var opToDB = map[ledgerv1.Operation]string{
	ledgerv1.Operation_OPERATION_DEPOSIT:  "deposit",
	ledgerv1.Operation_OPERATION_WITHDRAW: "withdraw",
	ledgerv1.Operation_OPERATION_BET:      "bet",
	ledgerv1.Operation_OPERATION_WIN:      "win",
	ledgerv1.Operation_OPERATION_ROLLBACK: "rollback",
	ledgerv1.Operation_OPERATION_HOLD:     "hold",
	ledgerv1.Operation_OPERATION_ADJUST:   "adjust",
}

var dbToOp = map[string]ledgerv1.Operation{
	"deposit":  ledgerv1.Operation_OPERATION_DEPOSIT,
	"withdraw": ledgerv1.Operation_OPERATION_WITHDRAW,
	"bet":      ledgerv1.Operation_OPERATION_BET,
	"win":      ledgerv1.Operation_OPERATION_WIN,
	"rollback": ledgerv1.Operation_OPERATION_ROLLBACK,
	"hold":     ledgerv1.Operation_OPERATION_HOLD,
	"adjust":   ledgerv1.Operation_OPERATION_ADJUST,
}

func kindFromCode(s string) ledgerv1.AccountKind { return dbToKind[s] }

func opCode(o ledgerv1.Operation) string { return opToDB[o] }

func opFromCode(s string) ledgerv1.Operation { return dbToOp[s] }
